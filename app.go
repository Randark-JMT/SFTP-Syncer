package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sftp-syncer/internal/config"
	"sftp-syncer/internal/syncer"
)

// 运行状态（与旧版保持一致的中文文案）。
const (
	StatusStopped  = "已停止"
	StatusRunning  = "运行中"
	StatusError    = "运行中（出错）"
	StatusStopping = "正在停止"
)

// 日志级别，与前端约定：0=信息 1=成功 2=警告 3=错误。
const (
	levelInfo    = 0
	levelSuccess = 1
	levelWarn    = 2
	levelError   = 3
)

const maxLogLines = 500

// LogEntry 是推送给前端的一条日志。
type LogEntry struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

type hostRun struct {
	cancel context.CancelFunc
	label  string
}

// App 是暴露给前端的桥接层。它持有配置存储与各主机的同步循环，
// 通过 Wails 事件把日志、进度与状态变化推送给前端。
type App struct {
	ctx context.Context

	mu        sync.Mutex // 保护 store、runs、statuses
	store     *config.Store
	runs      map[string]*hostRun
	statuses  map[string]string
	exiting   atomic.Bool
	exitOnce  sync.Once
	logMu     sync.Mutex
	logBuf    []LogEntry
}

func NewApp() *App {
	return &App{
		runs:     make(map[string]*hostRun),
		statuses: make(map[string]string),
	}
}

func (a *App) SetContext(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) IsExiting() bool {
	return a.exiting.Load()
}

// Startup 在窗口启动时加载配置并记录初始日志。
func (a *App) Startup() {
	store, err := config.LoadStore()
	if err != nil {
		a.appendLog(levelError, fmt.Sprintf("加载主机列表失败：%v", err))
	}
	if store == nil {
		store = &config.Store{}
	}
	a.mu.Lock()
	a.store = store
	for _, h := range store.Hosts {
		a.statuses[h.ID] = StatusStopped
	}
	a.mu.Unlock()

	if path, pathErr := config.StorePath(); pathErr == nil {
		a.appendLog(levelInfo, fmt.Sprintf("主机列表文件路径：%s", path))
	}
	if len(store.Hosts) == 0 {
		a.appendLog(levelInfo, "尚未添加主机。点击「添加主机」开始配置。")
	}
	a.appendLog(levelInfo, "应用已启动。关闭窗口时会隐藏到系统托盘。")
}

// ---------- 主机管理 ----------

// ListHosts 返回全部主机配置。
func (a *App) ListHosts() []config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]config.Config(nil), a.store.Hosts...)
}

// AddHost 校验并保存一台新主机，返回带 ID 的最终配置。
func (a *App) AddHost(cfg config.Config) (config.Config, error) {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}

	a.mu.Lock()
	added := a.store.Add(cfg)
	err := a.store.Save()
	a.statuses[added.ID] = StatusStopped
	a.mu.Unlock()
	if err != nil {
		return config.Config{}, err
	}

	a.appendLog(levelInfo, fmt.Sprintf("已添加主机：%s（%s:%d）。",
		added.DisplayName(), added.Host, added.Port))
	return added, nil
}

// UpdateHost 更新一台已存在的主机；运行中的主机不允许修改。
func (a *App) UpdateHost(cfg config.Config) error {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.ID == "" {
		return fmt.Errorf("缺少主机 ID")
	}

	a.mu.Lock()
	if _, running := a.runs[cfg.ID]; running {
		a.mu.Unlock()
		return fmt.Errorf("主机正在运行，请先停止同步任务后再编辑")
	}
	ok := a.store.Update(cfg)
	var err error
	if ok {
		err = a.store.Save()
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("主机不存在或已被删除")
	}
	if err != nil {
		return err
	}

	a.appendLog(levelInfo, fmt.Sprintf("已更新主机：%s。", cfg.DisplayName()))
	return nil
}

// RemoveHost 删除一台主机；运行中的主机不允许删除。
func (a *App) RemoveHost(id string) error {
	a.mu.Lock()
	if _, running := a.runs[id]; running {
		a.mu.Unlock()
		return fmt.Errorf("主机正在运行，请先停止同步任务后再删除")
	}
	cfg, found := a.store.Find(id)
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("主机不存在或已被删除")
	}
	a.store.Remove(id)
	delete(a.statuses, id)
	err := a.store.Save()
	a.mu.Unlock()
	if err != nil {
		return err
	}

	a.emitTasksClear(id)
	a.appendLog(levelInfo, fmt.Sprintf("已删除主机：%s。", cfg.DisplayName()))
	return nil
}

// HostStatuses 返回所有主机当前的运行状态。
func (a *App) HostStatuses() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.statuses))
	for id, s := range a.statuses {
		out[id] = s
	}
	return out
}

// ---------- 同步控制 ----------

// StartHost 启动一台主机的同步循环。
func (a *App) StartHost(id string) error {
	a.mu.Lock()
	cfg, ok := a.store.Find(id)
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("主机不存在或已被删除")
	}
	if _, running := a.runs[id]; running {
		a.mu.Unlock()
		return nil
	}

	label := cfg.DisplayName()
	ctx, cancel := context.WithCancel(context.Background())
	service := syncer.NewServiceLevel(func(level syncer.Level, format string, args ...any) {
		a.appendLog(syncerLevel(level), fmt.Sprintf("[%s] %s", label, fmt.Sprintf(format, args...)))
	})
	service.SetHost(cfg.ID, label)
	service.SetProgressCallback(func(evt syncer.ProgressEvent) {
		// 显式小写字段名映射：syncer.ProgressEvent 无 json 标签，
		// Wails 默认序列化会沿用 Go 导出字段名（HostID 等），与前端约定不符。
		runtime.EventsEmit(a.ctx, "progress", map[string]any{
			"hostId":     evt.HostID,
			"hostLabel":  evt.HostLabel,
			"remotePath": evt.RemotePath,
			"state":      string(evt.State),
			"downloaded": evt.Downloaded,
			"total":      evt.Total,
			"speedBps":   evt.SpeedBps,
		})
	})
	a.runs[id] = &hostRun{cancel: cancel, label: label}
	a.statuses[id] = StatusRunning
	a.mu.Unlock()

	a.emitStatus(id, StatusRunning)
	a.appendLog(levelInfo, fmt.Sprintf("[%s] 开始同步：服务器=%s:%d，远程目录=%s，本地目录=%s，轮询间隔=%d 秒。",
		label, cfg.Host, cfg.Port, cfg.RemoteDir, cfg.LocalDir, cfg.PollIntervalSeconds))
	go a.runHostLoop(ctx, cfg, service)
	return nil
}

// StopHost 请求停止一台主机的同步循环（实际停止是异步的）。
func (a *App) StopHost(id string) error {
	a.mu.Lock()
	run, running := a.runs[id]
	a.mu.Unlock()
	if !running {
		return nil
	}
	a.appendLog(levelInfo, fmt.Sprintf("[%s] 正在停止同步循环 ...", run.label))
	run.cancel()
	a.setStatus(id, StatusStopping)
	return nil
}

// StartAll 启动全部主机。
func (a *App) StartAll() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.store.Hosts))
	for _, h := range a.store.Hosts {
		ids = append(ids, h.ID)
	}
	a.mu.Unlock()
	for _, id := range ids {
		_ = a.StartHost(id)
	}
}

// StopAll 停止全部主机。
func (a *App) StopAll() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.runs))
	for id, run := range a.runs {
		ids = append(ids, id)
		run.cancel()
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.setStatus(id, StatusStopping)
	}
}

// RunningCount 返回正在运行的主机数量（含出错与停止中）。
func (a *App) RunningCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.runs)
}

// runHostLoop 是每台主机的轮询循环（对齐旧版 main_windows.go 行为）。
func (a *App) runHostLoop(ctx context.Context, cfg config.Config, service *syncer.Service) {
	label := cfg.DisplayName()
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second

	defer func() {
		a.mu.Lock()
		delete(a.runs, cfg.ID)
		a.mu.Unlock()
		a.emitTasksClear(cfg.ID)
		a.setStatus(cfg.ID, StatusStopped)
		if !a.exiting.Load() {
			a.appendLog(levelInfo, fmt.Sprintf("[%s] 同步循环已停止。", label))
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		a.emitTasksClear(cfg.ID)

		result, err := service.RunOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.appendLog(levelError, fmt.Sprintf("[%s] 本轮同步失败：%v", label, err))
			a.setStatus(cfg.ID, StatusError)
		} else {
			a.appendLog(levelInfo, fmt.Sprintf(
				"[%s] 本轮完成：扫描 %d 个文件，下载 %d，删除远程文件 %d，跳过根目录散文件 %d，跳过最近 30 分钟 %d，跳过非常规条目 %d，跳过隐藏目录 %d，本地已存在并直接删远程 %d。",
				label, result.Scanned, result.Downloaded, result.Deleted, result.SkippedRootFiles,
				result.SkippedRecent, result.SkippedNonRegular, result.SkippedHiddenDirs, result.AlreadyPresent))
			a.setStatus(cfg.ID, StatusRunning)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ---------- 退出 ----------

// Quit 停止全部同步并退出应用（托盘菜单「退出」调用）。
func (a *App) Quit() {
	a.exitOnce.Do(func() {
		a.exiting.Store(true)
		a.StopAll()
		go func() {
			// 给各循环一点时间完成清理，再退出托盘与主窗口。
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && a.RunningCount() > 0 {
				time.Sleep(50 * time.Millisecond)
			}
			systray.Quit()
			runtime.Quit(a.ctx)
		}()
	})
}

// ---------- 日志 ----------

// GetLogHistory 返回启动以来的历史日志（最多 500 条）。
func (a *App) GetLogHistory() []LogEntry {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	return append([]LogEntry(nil), a.logBuf...)
}

// ClearLog 清空日志历史。
func (a *App) ClearLog() {
	a.logMu.Lock()
	a.logBuf = nil
	a.logMu.Unlock()
}

// appendLog 线程安全地追加一条日志：写入环形缓冲并推送给前端。
func (a *App) appendLog(level int, message string) {
	entry := LogEntry{
		Level: level,
		Text:  fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), message),
	}

	a.logMu.Lock()
	a.logBuf = append(a.logBuf, entry)
	if len(a.logBuf) > maxLogLines {
		a.logBuf = append(a.logBuf[:0], a.logBuf[len(a.logBuf)-maxLogLines:]...)
	}
	a.logMu.Unlock()

	runtime.EventsEmit(a.ctx, "log", entry)
}

// ---------- 事件辅助 ----------

func (a *App) emitStatus(id, status string) {
	runtime.EventsEmit(a.ctx, "host-status", map[string]string{"id": id, "status": status})
}

func (a *App) emitTasksClear(hostID string) {
	runtime.EventsEmit(a.ctx, "tasks-clear", map[string]string{"hostId": hostID})
}

func (a *App) setStatus(id, status string) {
	a.mu.Lock()
	a.statuses[id] = status
	count := len(a.runs)
	total := len(a.store.Hosts)
	a.mu.Unlock()

	a.emitStatus(id, status)
	updateTrayTooltip(count, total)
}

// ---------- 文件/目录选择 ----------

// SelectDirectory 弹出系统目录选择框。
func (a *App) SelectDirectory(title string) (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}

// SelectFile 弹出系统文件选择框。
func (a *App) SelectFile(title string) (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}

// GetVersion 返回应用版本号。
func (a *App) GetVersion() string {
	return Version
}

// syncerLevel 把引擎的日志级别映射为前端约定。
func syncerLevel(l syncer.Level) int {
	switch l {
	case syncer.LevelSuccess:
		return levelSuccess
	case syncer.LevelWarn:
		return levelWarn
	case syncer.LevelError:
		return levelError
	default:
		return levelInfo
	}
}
