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
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/pkg/sftp"

	"sftp-syncer/internal/config"
)

const QuietPeriod = 30 * time.Minute

const (
	// maxDownloadWorkers 是单台主机允许配置的并发传输数上限。
	maxDownloadWorkers = 10
)

// workerRetryBackoff 是单个 Worker 在同一文件连续失败时的五级退避。
// 达到 10 秒上限后仍失败，才结束当前文件并接收队列中的下一个任务。
var workerRetryBackoff = [...]time.Duration{
	time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	10 * time.Second,
}

const (
	// keepaliveInterval / keepaliveTimeout 控制池化连接的 SSH keepalive：
	// 既防止 NAT/防火墙因空闲断开连接，也在对端失联时于有限时间内关闭
	// 连接，避免传输阻塞到 TCP 重传超时（可能长达数分钟），期间任务在
	// 前端只会显示"下载中"但毫无进展。
	keepaliveInterval = 15 * time.Second
	keepaliveTimeout  = 10 * time.Second

	// probeIdleThreshold 是复用池化连接前需要探活的空闲时长阈值；短于该
	// 阈值的连接由 keepalive 担保活性，直接复用。probeTimeout 是探活请求
	// 的硬超时，超时即关闭旧连接并重新拨号。
	probeIdleThreshold = 30 * time.Second
	probeTimeout       = 5 * time.Second

	// downloadStallTimeout 是下载停滞看门狗的触发时长：TCP 层存活但服务器
	// 不再发送数据时 keepalive 无法察觉，超过该时长没有任何字节到达即主动
	// 中止传输，随后由当前 Worker 按退避策略重试当前文件。
	downloadStallTimeout = 2 * time.Minute
)

const (
	// SFTP 一次 READ 请求的有效载荷上限决定了单连接的吞吐：带宽 ≈
	// 包大小 × 并发请求数 / RTT。协议标准要求服务器至少支持 32KB，默认值
	// 即 32KB，在高延迟链路上会成为明显瓶颈。256KB 是 OpenSSH 等主流
	// 服务器实际支持的上限（SSH_FXP_READ 的 Len 字段为 uint32，OpenSSH
	// 内部缓冲 256KB），配合更大的在途窗口可显著提高单文件速度。
	sftpMaxPacket = 256 * 1024
	// sftpMaxConcurrentRequests 是单个文件允许的在途读请求数上限，作为
	// 安全冗余设为下载窗口的两倍；实际在途请求由 downloadConcurrency
	// 的滑动窗口约束，不会触及该上限。
	sftpMaxConcurrentRequests = 64

	// downloadChunkSize 是本地下载器单个 ReadAt 请求的读块大小。
	downloadChunkSize = sftpMaxPacket
	// downloadConcurrency 是单个文件允许的在途读块数上限。下载器使用滑动
	// 窗口：读块按序写盘后才释放窗口槽位，单文件内存占用被严格限制在
	// downloadConcurrency × downloadChunkSize（32 × 256KB = 8MB），最多 10 个
	// Worker 同时下载约 80MB；旧实现 256 并发且乱序分块可无限堆积，10 个
	// Worker 峰值可超过 1GB。8MB 在途窗口在 200ms RTT 下仍提供约 40MB/s
	// 的单连接吞吐上限。
	downloadConcurrency = 32
)

type Logger func(format string, args ...any)

// Level is a log severity. The zero value is LevelInfo so a plain Logger
// keeps the historical INFO behaviour.
type Level int

const (
	LevelInfo Level = iota
	LevelSuccess
	LevelWarn
	LevelError
)

// Prefix returns the bracketed tag prepended to leveled messages, e.g.
// "[ERROR] ". UI layers key off this tag to colourise log lines.
func (l Level) Prefix() string {
	switch l {
	case LevelSuccess:
		return "[OK] "
	case LevelWarn:
		return "[WARN] "
	case LevelError:
		return "[ERROR] "
	default:
		return ""
	}
}

// LevelLogger is a severity-aware logger. Register it via NewServiceLevel to
// receive leveled messages natively instead of "[LEVEL] " text prefixes.
type LevelLogger func(level Level, format string, args ...any)

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
	queue   chan workerTask
	workers int
}

// poolWorker is a persistent goroutine that owns one SSH+SFTP connection.
// On failure it retries the current file with backoff before accepting new tasks.
type poolWorker struct {
	id        int
	service   *Service
	sshConfig *ssh.ClientConfig
	cfg       config.Config
	conn      *ssh.Client
	client    *sftp.Client

	// lastUsed 是池化连接最近一次成功使用的时间，connect 据此决定复用前
	// 是否需要探活。dead 由 keepalive 协程在对端失联时置位。stopKeepalive
	// 停止当前连接的 keepalive 协程，随 disconnect 清理。
	lastUsed      time.Time
	dead          atomic.Bool
	stopKeepalive func()
}

// ProgressState is the lifecycle state of a single file download task.
type ProgressState string

const (
	ProgressStatePending    ProgressState = "pending"
	ProgressStateConnecting ProgressState = "connecting"
	ProgressStateActive     ProgressState = "active"
	ProgressStateDone       ProgressState = "done"
	ProgressStateFailed     ProgressState = "failed"
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
	logger     LevelLogger
	now        func() time.Time
	onProgress ProgressCallback
	pool       *workerPool
	hostID     string
	hostLabel  string
}

// NewService wraps a plain Logger. Severity is encoded as a "[LEVEL] " text
// prefix so plain-log consumers can still distinguish severities.
func NewService(logger Logger) *Service {
	return NewServiceLevel(func(level Level, format string, args ...any) {
		if logger != nil {
			logger(level.Prefix()+format, args...)
		}
	})
}

// NewServiceLevel registers a severity-aware LevelLogger.
func NewServiceLevel(logger LevelLogger) *Service {
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
	s.initPool(ctx, sshConfig, cfg)
	if cfg.ProxyEnabled() {
		s.logf("通过 %s 代理 %s 连接到 %s ...", strings.ToUpper(cfg.ProxyMode), proxyAddr(cfg), addr)
	} else {
		s.logf("连接到 %s ...", addr)
	}
	conn, err := dialSSH(cfg, sshConfig)
	if err != nil {
		return Result{}, fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer conn.Close()

	// 下载阶段扫描连接会长时间空闲，启用 keepalive 防止被对端静默断开后
	// 下一轮遍历长时间阻塞。
	stopKeepalive := startSSHKeepalive(conn, nil)
	defer stopKeepalive()

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
			s.logWarn("遍历失败: %v", err)
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
			s.logWarn("无法计算本地路径（%s）: %v", remotePath, err)
			continue
		}

		if same, err := sameLocalFile(localPath, info); err == nil && same {
			if err := client.Remove(remotePath); err != nil {
				s.logWarn("本地已存在同名同时间文件，但删除远程文件失败（%s）: %v", remotePath, err)
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
		sftp.MaxPacketUnchecked(sftpMaxPacket),
		sftp.MaxConcurrentRequestsPerFile(sftpMaxConcurrentRequests),
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

	s.logf("本轮待下载 %d 个文件，提交至 %d 个 Worker 队列。", len(downloads), s.pool.workers)

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
func (s *Service) initPool(ctx context.Context, sshConfig *ssh.ClientConfig, cfg config.Config) {
	if s.pool != nil {
		return
	}
	cfg = cfg.Normalized()
	workerCount := cfg.Concurrency
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > maxDownloadWorkers {
		workerCount = maxDownloadWorkers
	}
	queue := make(chan workerTask, workerCount*256)
	s.pool = &workerPool{queue: queue, workers: workerCount}
	for i := range workerCount {
		w := &poolWorker{
			id:        i,
			service:   s,
			sshConfig: sshConfig,
			cfg:       cfg,
		}
		go w.run(ctx, queue)
	}
	s.logf("已启动 %d 个下载 Worker。", workerCount)
}

func (w *poolWorker) run(ctx context.Context, queue <-chan workerTask) {
	defer w.disconnect()

	for {
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

// connect 返回一条可用的 SFTP 连接：池化连接仍然可用时直接复用（空闲较久
// 先探活，keepalive 已判定失联则直接重连），否则重新拨号并为新连接启动
// keepalive。
func (w *poolWorker) connect() error {
	if w.client != nil && !w.dead.Load() {
		if time.Since(w.lastUsed) < probeIdleThreshold || w.probe() {
			return nil
		}
	}
	w.disconnect()

	conn, err := dialSSH(w.cfg, w.sshConfig)
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
	w.dead.Store(false)
	w.lastUsed = time.Now()
	w.stopKeepalive = startSSHKeepalive(conn, func() { w.dead.Store(true) })
	w.service.logf("Worker %d 已建立 SFTP 连接。", w.id+1)
	return nil
}

// probe 用一次 Stat 往返确认池化连接仍然可用。probeTimeout 超时即关闭底层
// 连接，使阻塞中的请求立即返回错误，而不是挂到 TCP 重传超时。
func (w *poolWorker) probe() bool {
	client, conn := w.client, w.conn
	done := make(chan error, 1)
	go func() {
		_, err := client.Stat(".")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			w.service.logf("Worker %d 池化连接探活失败，重新连接: %v", w.id+1, err)
			return false
		}
		return true
	case <-time.After(probeTimeout):
		w.service.logf("Worker %d 池化连接探活超时，重新连接。", w.id+1)
		_ = conn.Close()
		<-done
		return false
	}
}

func (w *poolWorker) disconnect() {
	if w.stopKeepalive != nil {
		w.stopKeepalive()
		w.stopKeepalive = nil
	}
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

// startSSHKeepalive 每隔 keepaliveInterval 发送一次 SSH keepalive 全局请求，
// 保持 NAT 会话存活；对端超过 keepaliveTimeout 未应答时主动关闭连接，让阻塞
// 中的 SFTP 请求快速失败而不是挂到 TCP 超时。onDead 在关闭连接前调用，可为
// nil。返回的 stop 函数用于停止 keepalive 协程。
func startSSHKeepalive(conn *ssh.Client, onDead func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}

			errCh := make(chan error, 1)
			go func() {
				_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
				errCh <- err
			}()
			select {
			case <-done:
				return
			case err := <-errCh:
				if err == nil {
					continue
				}
			case <-time.After(keepaliveTimeout):
			}

			if onDead != nil {
				onDead()
			}
			_ = conn.Close()
			return
		}
	}()
	return func() { close(done) }
}

func (w *poolWorker) enterCooldown(ctx context.Context, delay time.Duration, retryLevel int) bool {
	w.disconnect()
	w.service.logWarn("Worker %d 冷却 %v 后重试当前文件（退避 %d/%d）。", w.id+1, delay, retryLevel, len(workerRetryBackoff))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		w.service.logf("Worker %d 冷却结束，继续重试当前文件。", w.id+1)
		return true
	}
}

func (w *poolWorker) executeTask(ctx context.Context, task workerTask) {
	dl := task.dl

	if ctx.Err() != nil {
		task.result <- downloadOutcome{}
		return
	}

	if err := os.MkdirAll(filepath.Dir(dl.localPath), 0o755); err != nil {
		w.service.logError("创建本地目录失败（%s）: %v", dl.localPath, err)
		w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateFailed, Total: dl.info.Size()})
		task.result <- downloadOutcome{}
		return
	}

	// 拨号/探活或下载失败时保留当前任务，在同一 Worker 内进行五级退避重试；
	// 这样不会把一个暂时断连的文件切碎到队列末尾等待下一轮同步。
	var err error
	for retryLevel := 0; ; retryLevel++ {
		if ctx.Err() != nil {
			task.result <- downloadOutcome{}
			return
		}

		w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateConnecting, Total: dl.info.Size()})
		if err = w.connect(); err == nil {
			w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateActive, Total: dl.info.Size()})
			err = w.service.downloadFile(ctx, w.client, dl.remotePath, dl.localPath, dl.info)
		}
		if err == nil {
			break
		}
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			task.result <- downloadOutcome{}
			return
		}

		if retryLevel == len(workerRetryBackoff) {
			w.service.logError("Worker %d 当前文件连续重试 %d 级后仍失败（%s）: %v", w.id+1, len(workerRetryBackoff), dl.remotePath, err)
			w.service.notifyProgress(ProgressEvent{RemotePath: dl.remotePath, State: ProgressStateFailed, Total: dl.info.Size()})
			task.result <- downloadOutcome{}
			return
		}

		if w.client == nil {
			w.service.logWarn("Worker %d 连接失败（%s）: %v", w.id+1, dl.remotePath, err)
		} else {
			w.service.logWarn("Worker %d 下载失败（%s）: %v", w.id+1, dl.remotePath, err)
		}
		if !w.enterCooldown(ctx, workerRetryBackoff[retryLevel], retryLevel+1) {
			task.result <- downloadOutcome{}
			return
		}
	}

	w.lastUsed = time.Now()

	outcome := downloadOutcome{downloaded: true}
	if err := w.client.Remove(dl.remotePath); err != nil {
		w.service.logWarn("下载完成，但删除远程文件失败（%s）: %v", dl.remotePath, err)
	} else {
		outcome.deleted = true
		w.service.logSuccess("已同步并删除远程文件: %s -> %s", dl.remotePath, dl.localPath)
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

	// Re-stat the file so size validation does not rely on a stale size
	// captured during the (potentially long) directory scan.
	expectSize := info.Size()
	if fi, err := remoteFile.Stat(); err == nil && fi.Size() > 0 {
		expectSize = fi.Size()
	}

	tempPath := localPath + ".partial"
	_ = os.Remove(tempPath)

	localFile, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	s.logf("开始下载: %s -> %s (%d 字节)", remotePath, localPath, expectSize)

	// 停滞看门狗：TCP 层存活但服务器不再发数据时 keepalive 无法察觉，这里
	// 在长时间零字节后取消 stallCtx 中止传输，让当前 Worker 按退避策略重试，
	// 而不是无限期挂起。
	stallCtx, cancelStall := context.WithCancel(ctx)
	defer cancelStall()

	tracker := &progressTracker{
		lastTime: time.Now(),
		onUpdate: func(received int64, speedBps float64) {
			s.notifyProgress(ProgressEvent{
				RemotePath: remotePath,
				State:      ProgressStateActive,
				Downloaded: received,
				Total:      expectSize,
				SpeedBps:   speedBps,
			})
		},
	}
	tracker.lastActivity.Store(time.Now().UnixNano())

	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-stallCtx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, tracker.lastActivity.Load())) > downloadStallTimeout {
					s.logWarn("下载停滞超过 %v，中止传输: %s", downloadStallTimeout, remotePath)
					cancelStall()
					return
				}
			}
		}
	}()

	copied, copyErr := copyRemoteFile(stallCtx, remoteFile, localFile, expectSize, tracker.add)
	closeErr := localFile.Close()
	if copyErr != nil {
		if errors.Is(copyErr, context.Canceled) && ctx.Err() == nil {
			// 非用户取消，而是停滞看门狗触发：换成明确的错误，避免被上层
			// 误判为用户取消而静默跳过。
			copyErr = fmt.Errorf("下载停滞超过 %v 无进展", downloadStallTimeout)
		}
		_ = os.Remove(tempPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tempPath)
		return closeErr
	}
	if copied != expectSize {
		_ = os.Remove(tempPath)
		return fmt.Errorf("下载字节数不匹配，期望 %d，实际 %d", expectSize, copied)
	}
	s.logSuccess("下载完成: %s (%d 字节)", remotePath, copied)

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

// copyRemoteFile transfers an SFTP file to dst, honouring ctx cancellation.
// It first attempts a concurrent transfer when the file size is known and the
// file implements io.ReaderAt (true for *sftp.File); on a size mismatch from
// the concurrent path it retries once sequentially. If the concurrent path
// itself errors, that error is returned as-is (the destination content is
// indeterminate on error, and the caller removes the .partial file anyway).
//
// The concurrent path exists because pkg/sftp's built-in concurrent WriteTo
// treats any short SFTP read as io.EOF. SFTP READ replies are message-based
// and may legitimately return fewer bytes than requested; some servers do so
// under load. readAtFull closes that gap with io.ReadFull semantics.
func copyRemoteFile(ctx context.Context, src *sftp.File, dst io.Writer, size int64, onReceive func(int64)) (int64, error) {
	if size > 0 {
		written, err := copyRemoteFileConcurrent(ctx, src, dst, size, onReceive)
		if err != nil {
			return 0, err
		}
		if written == size {
			return written, nil
		}
		// The file changed size between the initial stat and the transfer.
		// Rewind and fall through to a sequential rewrite from scratch.
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}
	if onReceive != nil {
		dst = &receiveWriter{Writer: dst, onReceive: onReceive}
	}
	return copyWithContext(ctx, dst, src)
}

// chunkBufPool 复用下载读块缓冲，降低高吞吐下的 GC 压力。池内所有缓冲的
// 长度均为 downloadChunkSize。
var chunkBufPool = sync.Pool{
	New: func() any { return make([]byte, downloadChunkSize) },
}

// copyRemoteFileConcurrent downloads exactly size bytes from src into dst
// using bounded concurrent ReadAt calls (io.ReadFull semantics), writing the
// chunks to dst strictly in offset order so dst needs no Seek. It is
// cancellation-aware via ctx: on cancellation the reader is closed, which
// aborts the in-flight SFTP requests, and context.Canceled is returned.
//
// onReceive（可为 nil）在每个分块从网络到达时按实际收到的字节数回调；分块
// 乱序完成，回调顺序与文件偏移无关，调用方需自行保证并发安全。
func copyRemoteFileConcurrent(ctx context.Context, src io.ReadCloser, dst io.Writer, size int64, onReceive func(int64)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Close src on cancellation so in-flight ReadAt calls abort promptly.
	stop := context.AfterFunc(ctx, func() { _ = src.Close() })
	defer stop()

	ra, ok := src.(io.ReaderAt)
	if !ok {
		return 0, errors.New("reader does not support ReadAt")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	nChunks := int((size + downloadChunkSize - 1) / downloadChunkSize)

	type chunkResult struct {
		index int
		data  []byte
		buf   []byte // 完整的池化缓冲，写盘后归还 chunkBufPool
		err   error
	}

	// 滑动窗口：window 统计"已派发但未写盘"的分块数，槽位只在分块写盘后
	// 释放，因此内存占用被严格限制在 downloadConcurrency ×
	// downloadChunkSize，不随文件大小增长；乱序完成的分块暂存于 backlog，
	// 同样受窗口约束。
	window := make(chan struct{}, downloadConcurrency)
	jobs := make(chan int)
	completed := make(chan chunkResult, downloadConcurrency)

	var wg sync.WaitGroup
	for range downloadConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					continue
				}
				off := int64(index) * downloadChunkSize
				end := min(off+downloadChunkSize, size)
				buf := chunkBufPool.Get().([]byte)
				n, err := readAtFull(ra, buf[:end-off], off)
				if err == nil {
					err = ctx.Err()
				}
				if n > 0 && onReceive != nil {
					onReceive(int64(n))
				}
				select {
				case completed <- chunkResult{index: index, data: buf[:n], buf: buf, err: err}:
				case <-ctx.Done():
					chunkBufPool.Put(buf)
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := 0; i < nChunks; i++ {
			select {
			case <-ctx.Done():
				return
			case window <- struct{}{}:
			}
			select {
			case <-ctx.Done():
				return
			case jobs <- i:
			}
		}
	}()

	backlog := make(map[int]chunkResult, downloadConcurrency)
	written := int64(0)
	for next := 0; next < nChunks; {
		var r chunkResult
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		case r = <-completed:
		}
		backlog[r.index] = r
		for {
			rc, ok := backlog[next]
			if !ok {
				break
			}
			delete(backlog, next)
			<-window
			if len(rc.data) > 0 {
				n, err := dst.Write(rc.data)
				written += int64(n)
				if err != nil {
					return written, err
				}
				if n != len(rc.data) {
					return written, io.ErrShortWrite
				}
			}
			chunkBufPool.Put(rc.buf)
			if rc.err != nil {
				if ctx.Err() != nil {
					return written, ctx.Err()
				}
				return written, rc.err
			}
			next++
		}
	}

	wg.Wait()
	return written, ctx.Err()
}

// readAtFull reads exactly len(b) bytes from ra starting at off, retrying
// short reads. Unlike io.ReadFull with an arbitrary ReaderAt, EOF before any
// byte is read returns io.EOF, and EOF after partial data returns
// io.ErrUnexpectedEOF.
func readAtFull(ra io.ReaderAt, b []byte, off int64) (int, error) {
	total := 0
	for total < len(b) {
		n, err := ra.ReadAt(b[total:], off+int64(total))
		total += n
		if err != nil {
			if err == io.EOF && total > 0 {
				return total, io.ErrUnexpectedEOF
			}
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
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

// progressTracker 按"已从网络收到的字节数"统计进度，而不是按写入磁盘的顺序
// 统计：并发下载时后偏移的分块可能先落在内存里，按写盘顺序统计会让进度条
// 长时间停在 0 再猛跳，看起来像"卡住很久才开始"。onUpdate 最多每 250ms 触发
// 一次。add 可被多个分块协程并发调用。
type progressTracker struct {
	mu        sync.Mutex
	received  int64
	lastBytes int64
	lastTime  time.Time
	onUpdate  func(received int64, speedBps float64)

	// lastActivity 记录最近一次收到字节的时间（UnixNano），供停滞看门狗
	// 判断传输是否还活着。
	lastActivity atomic.Int64
}

func (pt *progressTracker) add(n int64) {
	if n <= 0 {
		return
	}
	pt.lastActivity.Store(time.Now().UnixNano())
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.received += n
	now := time.Now()
	if elapsed := now.Sub(pt.lastTime); elapsed >= 250*time.Millisecond {
		delta := pt.received - pt.lastBytes
		pt.onUpdate(pt.received, float64(delta)/elapsed.Seconds())
		pt.lastBytes = pt.received
		pt.lastTime = now
	}
}

// receiveWriter 把顺序拷贝路径的字节数桥接到 onReceive 回调。
type receiveWriter struct {
	io.Writer
	onReceive func(int64)
}

func (rw *receiveWriter) Write(p []byte) (int, error) {
	n, err := rw.Writer.Write(p)
	if n > 0 {
		rw.onReceive(int64(n))
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
	s.logLevel(LevelInfo, format, args...)
}

// logLevel logs at the given severity.
func (s *Service) logLevel(level Level, format string, args ...any) {
	if s.logger != nil {
		s.logger(level, format, args...)
	}
}

func (s *Service) logSuccess(format string, args ...any) { s.logLevel(LevelSuccess, format, args...) }
func (s *Service) logWarn(format string, args ...any)    { s.logLevel(LevelWarn, format, args...) }
func (s *Service) logError(format string, args ...any)   { s.logLevel(LevelError, format, args...) }
