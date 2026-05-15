package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/pkg/sftp"

	"sftp-syncer/internal/config"
)

const QuietPeriod = 30 * time.Minute

const (
	maxDownloadWorkers           = 4
	maxConcurrentRequestsPerFile = 128
)

type Logger func(format string, args ...any)

type Result struct {
	Scanned           int
	Downloaded        int
	Deleted           int
	SkippedRecent     int
	SkippedNonRegular int
	SkippedRootFiles  int
	AlreadyPresent    int
}

type pendingDownload struct {
	remotePath string
	localPath  string
	info       os.FileInfo
}

type downloadOutcome struct {
	downloaded bool
	deleted    bool
}

type Service struct {
	logger Logger
	now    func() time.Time
}

func NewService(logger Logger) *Service {
	return &Service{
		logger: logger,
		now:    time.Now,
	}
}

func (s *Service) RunOnce(ctx context.Context, cfg config.Config) (Result, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}

	sshConfig, err := s.buildSSHConfig(cfg)
	if err != nil {
		return Result{}, err
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	s.logf("连接到 %s ...", addr)
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return Result{}, fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer conn.Close()

	client, err := s.newSFTPClient(conn)
	if err != nil {
		return Result{}, fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}
	defer client.Close()

	cutoff := s.now().UTC().Add(-QuietPeriod)
	remoteRoot := normalizeRemotePath(cfg.RemoteDir)
	localRoot := filepath.Clean(cfg.LocalDir)

	s.logf("开始扫描远程目录 %s，筛选 UTC 时间早于 %s 的文件。", remoteRoot, cutoff.Format(time.RFC3339))

	walker := client.Walk(remoteRoot)
	var result Result
	var pendingDownloads []pendingDownload
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		if err := walker.Err(); err != nil {
			s.logf("遍历失败: %v", err)
			continue
		}

		info := walker.Stat()
		if info == nil {
			continue
		}

		if info.IsDir() {
			continue
		}

		result.Scanned++
		remotePath := normalizeRemotePath(walker.Path())

		if !info.Mode().IsRegular() {
			result.SkippedNonRegular++
			s.logf("跳过非常规条目: %s", remotePath)
			continue
		}

		if !isFileInChildDirectory(remoteRoot, remotePath) {
			result.SkippedRootFiles++
			s.logf("跳过根目录下的文件（仅同步子文件夹内容）: %s", remotePath)
			continue
		}

		modTimeUTC := info.ModTime().UTC()
		if !eligibleForTransfer(modTimeUTC, s.now().UTC(), QuietPeriod) {
			result.SkippedRecent++
			continue
		}

		localPath, err := targetLocalPath(localRoot, remoteRoot, remotePath)
		if err != nil {
			s.logf("无法计算本地路径（%s）: %v", remotePath, err)
			continue
		}

		if same, err := sameLocalFile(localPath, info); err == nil && same {
			if err := client.Remove(remotePath); err != nil {
				s.logf("本地已存在同名同时间文件，但删除远程文件失败（%s）: %v", remotePath, err)
				continue
			}
			result.AlreadyPresent++
			result.Deleted++
			s.logf("远程文件已在本地存在，直接删除远程文件: %s", remotePath)
			continue
		}

		pendingDownloads = append(pendingDownloads, pendingDownload{
			remotePath: remotePath,
			localPath:  localPath,
			info:       info,
		})
	}

	if err := walker.Err(); err != nil && !errors.Is(err, io.EOF) {
		return result, fmt.Errorf("远程遍历结束时出错: %w", err)
	}

	downloaded, deleted, err := s.processDownloads(ctx, conn, client, pendingDownloads)
	result.Downloaded += downloaded
	result.Deleted += deleted
	if err != nil {
		return result, err
	}

	return result, nil
}

func (s *Service) newSFTPClient(conn *ssh.Client) (*sftp.Client, error) {
	return sftp.NewClient(
		conn,
		sftp.UseConcurrentReads(true),
		sftp.UseFstat(true),
		sftp.MaxConcurrentRequestsPerFile(maxConcurrentRequestsPerFile),
	)
}

func (s *Service) processDownloads(ctx context.Context, conn *ssh.Client, primaryClient *sftp.Client, downloads []pendingDownload) (int, int, error) {
	if len(downloads) == 0 {
		return 0, 0, nil
	}

	clients, cleanup := s.downloadClients(conn, primaryClient, len(downloads))
	defer cleanup()

	s.logf("本轮待下载 %d 个文件，启用 %d 条下载通道。", len(downloads), len(clients))

	tasks := make(chan pendingDownload, len(clients))
	outcomes := make(chan downloadOutcome, len(downloads))

	var wg sync.WaitGroup
	for _, workerClient := range clients {
		wg.Add(1)
		go func(client *sftp.Client) {
			defer wg.Done()

			for task := range tasks {
				if err := ctx.Err(); err != nil {
					return
				}

				if err := os.MkdirAll(filepath.Dir(task.localPath), 0o755); err != nil {
					s.logf("创建本地目录失败（%s）: %v", task.localPath, err)
					continue
				}

				if err := s.downloadFile(ctx, client, task.remotePath, task.localPath, task.info); err != nil {
					s.logf("下载失败（%s）: %v", task.remotePath, err)
					continue
				}

				outcome := downloadOutcome{downloaded: true}
				if err := client.Remove(task.remotePath); err != nil {
					s.logf("下载完成，但删除远程文件失败（%s）: %v", task.remotePath, err)
					outcomes <- outcome
					continue
				}

				outcome.deleted = true
				s.logf("已同步并删除远程文件: %s -> %s", task.remotePath, task.localPath)
				outcomes <- outcome
			}
		}(workerClient)
	}

	go func() {
		defer close(tasks)
		for _, task := range downloads {
			select {
			case <-ctx.Done():
				return
			case tasks <- task:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(outcomes)
	}()

	var downloaded int
	var deleted int
	for outcome := range outcomes {
		if outcome.downloaded {
			downloaded++
		}
		if outcome.deleted {
			deleted++
		}
	}

	if err := ctx.Err(); err != nil {
		return downloaded, deleted, err
	}

	return downloaded, deleted, nil
}

func (s *Service) downloadClients(conn *ssh.Client, primaryClient *sftp.Client, taskCount int) ([]*sftp.Client, func()) {
	clients := []*sftp.Client{primaryClient}
	workerCount := downloadWorkerCount(taskCount)
	if workerCount <= 1 {
		return clients, func() {}
	}

	extras := make([]*sftp.Client, 0, workerCount-1)
	for i := 1; i < workerCount; i++ {
		client, err := s.newSFTPClient(conn)
		if err != nil {
			s.logf("创建额外下载通道失败，降级为 %d 条下载通道: %v", len(clients), err)
			break
		}

		clients = append(clients, client)
		extras = append(extras, client)
	}

	return clients, func() {
		for _, client := range extras {
			_ = client.Close()
		}
	}
}

func downloadWorkerCount(taskCount int) int {
	if taskCount <= 0 {
		return 0
	}
	if taskCount < maxDownloadWorkers {
		return taskCount
	}
	return maxDownloadWorkers
}

func (s *Service) buildSSHConfig(cfg config.Config) (*ssh.ClientConfig, error) {
	callback, err := s.hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	authMethod, err := s.authMethod(cfg)
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            []ssh.AuthMethod{authMethod},
		HostKeyCallback: callback,
		Timeout:         15 * time.Second,
	}, nil
}

func (s *Service) authMethod(cfg config.Config) (ssh.AuthMethod, error) {
	switch cfg.AuthMode {
	case config.AuthModePassword:
		s.logf("使用密码认证。")
		return ssh.Password(cfg.Password), nil
	case config.AuthModePrivateKey:
		s.logf("使用私钥认证：%s", cfg.PrivateKeyPath)
		return s.privateKeyAuthMethod(cfg)
	default:
		return nil, fmt.Errorf("不支持的认证方式：%s", cfg.AuthMode)
	}
}

func (s *Service) privateKeyAuthMethod(cfg config.Config) (ssh.AuthMethod, error) {
	privateKeyBytes, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("读取私钥失败（%s）: %w", cfg.PrivateKeyPath, err)
	}

	var signer ssh.Signer
	if cfg.PrivateKeyPassphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(privateKeyBytes, []byte(cfg.PrivateKeyPassphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(privateKeyBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败（%s）: %w", cfg.PrivateKeyPath, err)
	}

	return ssh.PublicKeys(signer), nil
}

func (s *Service) hostKeyCallback(cfg config.Config) (ssh.HostKeyCallback, error) {
	if cfg.SkipHostKeyValidation {
		s.logf("已启用跳过主机密钥校验。")
		return ssh.InsecureIgnoreHostKey(), nil
	}

	knownHostsPath, err := config.EffectiveKnownHostsPath(cfg)
	if err != nil {
		return nil, err
	}

	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("加载 known_hosts 失败（%s）: %w", knownHostsPath, err)
	}
	return callback, nil
}

func (s *Service) downloadFile(ctx context.Context, client *sftp.Client, remotePath, localPath string, info os.FileInfo) error {
	err := s.downloadFileOnce(ctx, client, remotePath, localPath, info, true)
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}

	s.logf("高速下载失败，改用兼容模式重试（%s）: %v", remotePath, err)
	return s.downloadFileOnce(ctx, client, remotePath, localPath, info, false)
}

func (s *Service) downloadFileOnce(ctx context.Context, client *sftp.Client, remotePath, localPath string, info os.FileInfo, allowWriterTo bool) error {
	remoteFile, err := client.Open(remotePath)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	tempPath := localPath + ".partial"
	_ = os.Remove(tempPath)

	localFile, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	modeName := "高速模式"
	if !allowWriterTo {
		modeName = "兼容模式"
	}
	s.logf("开始下载（%s）: %s -> %s (%d 字节)", modeName, remotePath, localPath, info.Size())

	copied, copyErr := copyWithContextMode(ctx, localFile, remoteFile, allowWriterTo)
	closeErr := localFile.Close()
	if copyErr != nil {
		_ = os.Remove(tempPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tempPath)
		return closeErr
	}
	if copied != info.Size() {
		_ = os.Remove(tempPath)
		return fmt.Errorf("下载字节数不匹配，期望 %d，实际 %d", info.Size(), copied)
	}
	s.logf("下载完成（%s）: %s (%d 字节)", modeName, remotePath, copied)

	if err := os.Remove(localPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, localPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}

	modTimeUTC := info.ModTime().UTC()
	if err := os.Chtimes(localPath, modTimeUTC, modTimeUTC); err != nil {
		return err
	}

	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return copyWithContextMode(ctx, dst, src, true)
}

func copyWithContextMode(ctx context.Context, dst io.Writer, src io.Reader, allowWriterTo bool) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if allowWriterTo {
		if writerTo, ok := src.(io.WriterTo); ok {
			var stop func() bool
			if closer, ok := src.(io.Closer); ok {
				stop = context.AfterFunc(ctx, func() {
					_ = closer.Close()
				})
			}
			if stop != nil {
				defer stop()
			}

			written, err := writerTo.WriteTo(dst)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return written, ctxErr
				}
				return written, err
			}
			return written, nil
		}
	}

	buf := make([]byte, 32*1024)
	var written int64
	var emptyReads int

	const maxConsecutiveEmptyReads = 100

	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			emptyReads = 0
			nw, ew := dst.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		} else if er == nil {
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyReads {
				return written, io.ErrNoProgress
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return written, nil
			}
			return written, er
		}
	}
}

func eligibleForTransfer(modTimeUTC, nowUTC time.Time, quietPeriod time.Duration) bool {
	return !modTimeUTC.After(nowUTC.UTC().Add(-quietPeriod))
}

func targetLocalPath(localRoot, remoteRoot, remotePath string) (string, error) {
	rel, err := relativeRemotePath(remoteRoot, remotePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(localRoot, filepath.FromSlash(rel)), nil
}

func isFileInChildDirectory(remoteRoot, remotePath string) bool {
	rel, err := relativeRemotePath(remoteRoot, remotePath)
	if err != nil {
		return false
	}
	return strings.Contains(rel, "/")
}

func relativeRemotePath(remoteRoot, remotePath string) (string, error) {
	rootSegs := remoteSegments(remoteRoot)
	pathSegs := remoteSegments(remotePath)
	if len(pathSegs) < len(rootSegs) {
		return "", fmt.Errorf("路径 %s 不在根目录 %s 之下", remotePath, remoteRoot)
	}
	for i := range rootSegs {
		if rootSegs[i] != pathSegs[i] {
			return "", fmt.Errorf("路径 %s 不在根目录 %s 之下", remotePath, remoteRoot)
		}
	}
	if len(pathSegs) == len(rootSegs) {
		return ".", nil
	}
	return strings.Join(pathSegs[len(rootSegs):], "/"), nil
}

func remoteSegments(p string) []string {
	cleaned := normalizeRemotePath(p)
	if cleaned == "." || cleaned == "/" {
		return nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	return strings.Split(cleaned, "/")
}

func normalizeRemotePath(p string) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(p, `\`, `/`))
	if trimmed == "" {
		return "."
	}
	return path.Clean(trimmed)
}

func sameLocalFile(localPath string, remoteInfo os.FileInfo) (bool, error) {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	return localInfo.Size() == remoteInfo.Size() && localInfo.ModTime().UTC().Equal(remoteInfo.ModTime().UTC()), nil
}

func (s *Service) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger(format, args...)
	}
}
