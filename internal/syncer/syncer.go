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
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/pkg/sftp"

	"sftp-syncer/internal/config"
)

const QuietPeriod = 30 * time.Minute

const (
	maxDownloadWorkers = 4
	workerCooldown     = 10 * time.Second
)

type Logger func(format string, args ...any)

type Result struct {
	Scanned           int
	Downloaded        int
	Deleted           int
	SkippedRecent     int
	SkippedNonRegular int
	SkippedRootFiles  int
	SkippedHiddenDirs int
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

// workerTask is a download job submitted to the persistent worker pool.
type workerTask struct {
	dl     pendingDownload
	result chan<- downloadOutcome
}

// workerPool holds the shared task queue consumed by poolWorkers.
type workerPool struct {
	queue chan workerTask
}

// poolWorker is a persistent goroutine that owns one SSH+SFTP connection.
// On any failure it disconnects and enters a cooldown before accepting new tasks.
type poolWorker struct {
	id        int
	service   *Service
	sshConfig *ssh.ClientConfig
	addr      string
	conn      *ssh.Client
	client    *sftp.Client
	coolUntil time.Time
}

// ProgressState is the lifecycle state of a single file download task.
type ProgressState string

const (
	ProgressStatePending ProgressState = "pending"
	ProgressStateActive  ProgressState = "active"
	ProgressStateDone    ProgressState = "done"
	ProgressStateFailed  ProgressState = "failed"
)

// ProgressEvent carries per-file progress information to the caller.
// HostID and HostLabel identify which managed host the event belongs to.
type ProgressEvent struct {
	HostID     string
	HostLabel  string
	RemotePath string
	State      ProgressState
	Downloaded int64
	Total      int64
	SpeedBps   float64
}

// ProgressCallback is invoked with progress updates during downloads.
type ProgressCallback func(ProgressEvent)

type Service struct {
	logger     Logger
	now        func() time.Time
	onProgress ProgressCallback
	pool       *workerPool
	hostID     string
	hostLabel  string
}

func NewService(logger Logger) *Service {
	return &Service{
		logger: logger,
		now:    time.Now,
	}
}

// SetHost records the host identity stamped onto progress events. It must be
// called before RunOnce so that pool workers observe stable values.
func (s *Service) SetHost(id, label string) {
	s.hostID = id
	s.hostLabel = label
}

// SetProgressCallback registers a callback for per-file download progress updates.
func (s *Service) SetProgressCallback(cb ProgressCallback) {
	s.onProgress = cb
}

func (s *Service) notifyProgress(evt ProgressEvent) {
	if s.onProgress == nil {
		return
	}
	evt.HostID = s.hostID
	evt.HostLabel = s.hostLabel
	s.onProgress(evt)
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
	s.initPool(ctx, sshConfig, addr)
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
			if hiddenRemoteDir(remoteRoot, walker.Path()) {
				result.SkippedHiddenDirs++
				s.logf("跳过以点开头的目录: %s", normalizeRemotePath(walker.Path()))
				walker.SkipDir()
			}
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

	downloaded, deleted, err := s.processDownloads(ctx, pendingDownloads)
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
	)
}

func (s *Service) processDownloads(ctx context.Context, downloads []pendingDownload) (int, int, error) {
	if len(downloads) == 0 {
		return 0, 0, nil
	}

	for _, dl := range downloads {
		s.notifyProgress(ProgressEvent{
			RemotePath: dl.remotePath,
			State:      ProgressStatePending,
			Total:      dl.info.Size(),
		})
	}

	s.logf("本轮待下载 %d 个文件，提交至 %d 个 Worker 队列。", len(downloads), maxDownloadWorkers)

	resultCh := make(chan downloadOutcome, len(downloads))
	for _, dl := range downloads {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case s.pool.queue <- workerTask{dl: dl, result: resultCh}:
		}
	}

	var downloaded, deleted int
	for range downloads {
		select {
		case <-ctx.Done():
			return downloaded, deleted, ctx.Err()
		case outcome := <-resultCh:
			if outcome.downloaded {
				downloaded++
			}
			if outcome.deleted {
				deleted++
			}
		}
	}

	return downloaded, deleted, nil
}

// initPool initialises the persistent worker pool on the first call; subsequent
// calls are no-ops. Workers are bound to ctx so they stop when sync is cancelled.
func (s *Service) initPool(ctx context.Context, sshConfig *ssh.ClientConfig, addr string) {
	if s.pool != nil {
		return
	}
	queue := make(chan workerTask, maxDownloadWorkers*256)
	s.pool = &workerPool{queue: queue}
	for i := range maxDownloadWorkers {
		w := &poolWorker{
			id:        i,
			service:   s,
			sshConfig: sshConfig,
			addr:      addr,
		}
		go w.run(ctx, queue)
	}
	s.logf("已启动 %d 个下载 Worker。", maxDownloadWorkers)
}

func (w *poolWorker) run(ctx context.Context, queue <-chan workerTask) {
	defer w.disconnect()

	for {
		if delay := time.Until(w.coolUntil); delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
				w.service.logf("Worker %d 冷却结束，恢复接收任务。", w.id+1)
			}
		}

		select {
		case <-ctx.Done():
			return
		case task, ok := <-queue:
			if !ok {
				return
			}
			w.executeTask(ctx, task)
		}
	}
}

func (w *poolWorker) connect() error {
	if w.client != nil {
		return nil
	}
	conn, err := ssh.Dial("tcp", w.addr, w.sshConfig)
	if err != nil {
		return fmt.Errorf("SSH 连接失败: %w", err)
	}
	client, err := w.service.newSFTPClient(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}
	w.conn = conn
	w.client = client
	w.service.logf("Worker %d 已建立 SFTP 连接。", w.id+1)
	return nil
}

func (w *poolWorker) disconnect() {
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

func (w *poolWorker) enterCooldown() {
	w.coolUntil = time.Now().Add(workerCooldown)
	w.service.logf("Worker %d 进入 %v 冷却。", w.id+1, workerCooldown)
	w.disconnect()
}

func (w *poolWorker) executeTask(ctx context.Context, task workerTask) {
	dl := task.dl

	if ctx.Err() != nil {
		task.result <- downloadOutcome{}
		return
	}

	if err := os.MkdirAll(filepath.Dir(dl.localPath), 0o755); err != nil {
		w.service.logf("创建本地目录失败（%s）: %v", dl.localPath, err)
		w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateFailed, Total: dl.info.Size()})
		task.result <- downloadOutcome{}
		return
	}

	w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateActive, Total: dl.info.Size()})

	if err := w.connect(); err != nil {
		w.service.logf("Worker %d 连接失败，进入冷却（%s）: %v", w.id+1, dl.remotePath, err)
		w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateFailed, Total: dl.info.Size()})
		w.enterCooldown()
		task.result <- downloadOutcome{}
		return
	}

	if err := w.service.downloadFile(ctx, w.client, dl.remotePath, dl.localPath, dl.info); err != nil {
		if errors.Is(err, context.Canceled) {
			task.result <- downloadOutcome{}
			return
		}
		w.service.logf("Worker %d 下载失败，进入冷却（%s）: %v", w.id+1, dl.remotePath, err)
		w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateFailed, Total: dl.info.Size()})
		w.enterCooldown()
		task.result <- downloadOutcome{}
		return
	}

	outcome := downloadOutcome{downloaded: true}
	if err := w.client.Remove(dl.remotePath); err != nil {
		w.service.logf("下载完成，但删除远程文件失败（%s）: %v", dl.remotePath, err)
	} else {
		outcome.deleted = true
		w.service.logf("已同步并删除远程文件: %s -> %s", dl.remotePath, dl.localPath)
	}

	w.service.notifyProgress(ProgressEvent{
		RemotePath: dl.remotePath,
		State:      ProgressStateDone,
		Downloaded: dl.info.Size(),
		Total:      dl.info.Size(),
	})
	task.result <- outcome
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
	s.logf("开始下载: %s -> %s (%d 字节)", remotePath, localPath, info.Size())

	pw := &progressWriter{
		Writer:   localFile,
		lastTime: time.Now(),
		onUpdate: func(downloaded int64, speedBps float64) {
			s.notifyProgress(ProgressEvent{
				RemotePath: remotePath,
				State:      ProgressStateActive,
				Downloaded: downloaded,
				Total:      info.Size(),
				SpeedBps:   speedBps,
			})
		},
	}

	copied, copyErr := copyWithContext(ctx, pw, remoteFile)
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
	s.logf("下载完成: %s (%d 字节)", remotePath, copied)

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

// copyWithContext copies src → dst, preferring the WriterTo fast path when
// available (used by sftp.File for pipelined reads). Falls back to a buffered
// Read loop for any reader that does not implement WriterTo.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

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

// progressWriter wraps an io.Writer and calls onUpdate with byte count and speed
// at most once every 250 ms so callers are not flooded with callbacks.
type progressWriter struct {
	io.Writer
	downloaded int64
	lastBytes  int64
	lastTime   time.Time
	onUpdate   func(downloaded int64, speedBps float64)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.Writer.Write(p)
	pw.downloaded += int64(n)
	now := time.Now()
	if elapsed := now.Sub(pw.lastTime); elapsed >= 250*time.Millisecond {
		delta := pw.downloaded - pw.lastBytes
		pw.onUpdate(pw.downloaded, float64(delta)/elapsed.Seconds())
		pw.lastBytes = pw.downloaded
		pw.lastTime = now
	}
	return n, err
}

func eligibleForTransfer(modTimeUTC, nowUTC time.Time, quietPeriod time.Duration) bool {
	return !modTimeUTC.After(nowUTC.UTC().Add(-quietPeriod))
}

// hiddenRemoteDir reports whether dirPath names a dot-prefixed directory
// underneath remoteRoot. The root itself is never treated as hidden, so a
// root such as /data/.incoming keeps working.
func hiddenRemoteDir(remoteRoot, dirPath string) bool {
	normalized := normalizeRemotePath(dirPath)
	if normalized == remoteRoot {
		return false
	}
	return strings.HasPrefix(path.Base(normalized), ".")
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
