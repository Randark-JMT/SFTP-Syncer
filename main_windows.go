//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"sftp-syncer/internal/config"
	"sftp-syncer/internal/syncer"
)

// Run statuses shown in the host table.
const (
	hostStatusStopped  = "已停止"
	hostStatusRunning  = "运行中"
	hostStatusError    = "运行中（出错）"
	hostStatusStopping = "正在停止"
)

type appWindow struct {
	*walk.MainWindow

	// store is the persisted host list. It is only mutated on the UI thread.
	store *config.Store

	hostModel *hostTableModel
	hostView  *walk.TableView

	addHostButton    *walk.PushButton
	editHostButton   *walk.PushButton
	removeHostButton *walk.PushButton
	startHostButton  *walk.PushButton
	stopHostButton   *walk.PushButton
	startAllButton   *walk.PushButton
	stopAllButton    *walk.PushButton

	statusLabel         *walk.Label
	logView             *logView
	taskView            *walk.TableView
	taskModel           *fileTaskModel
	tray                *walk.NotifyIcon
	trayHintShown       bool
	logLines            []logLine
	exiting             bool
	allowMinimizeToTray bool

	// syncMu guards runs; store access stays on the UI thread.
	syncMu          sync.Mutex
	runs            map[string]*hostRun // hostID → active sync loop
	taskRefreshStop chan struct{}
}

// hostRun tracks the active sync loop of one host.
type hostRun struct {
	cancel context.CancelFunc
}

func main() {
	window, err := newAppWindow()
	if err != nil {
		showStartupError(err)
		return
	}
	defer func() {
		if window.tray != nil {
			_ = window.tray.Dispose()
		}
	}()

	window.loadHosts()
	window.Starting().Attach(func() {
		win.ShowWindow(window.Handle(), win.SW_SHOWNORMAL)
		window.appendLog("应用已启动。最小化或关闭窗口时会隐藏到系统托盘。")
		window.allowMinimizeToTray = true
		window.startTaskRefresh()
	})

	window.Run()
}

func showStartupError(err error) {
	log.Printf("启动 GUI 失败: %v", err)
	walk.MsgBox(nil, "SFTP Syncer 启动失败", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func newAppWindow() (*appWindow, error) {
	aw := &appWindow{
		store:     &config.Store{},
		hostModel: newHostTableModel(),
		taskModel: newFileTaskModel(),
		runs:      make(map[string]*hostRun),
	}
	if err := aw.buildUI(); err != nil {
		return nil, err
	}
	if err := aw.initTray(); err != nil {
		aw.Dispose()
		return nil, err
	}
	aw.bindEvents()
	aw.updateHostButtons()
	return aw, nil
}

func (aw *appWindow) buildUI() error {
	return (MainWindow{
		AssignTo: &aw.MainWindow,
		Name:     "SFTPSyncerMainWindow",
		Title:    "SFTP Syncer " + Version,
		MinSize:  Size{Width: 860, Height: 620},
		Size:     Size{Width: 1000, Height: 780},
		Layout:   VBox{},
		Children: []Widget{
			GroupBox{
				Title:  "主机管理",
				Layout: VBox{},
				Children: []Widget{
					TableView{
						AssignTo:       &aw.hostView,
						Model:          aw.hostModel,
						MultiSelection: true, // walk only syncs SelectedIndexes() on clicks in multi-selection mode
						Columns: []TableViewColumn{
							{Title: "名称", Width: 140},
							{Title: "服务器", Width: 150},
							{Title: "远程目录", Width: 150},
							{Title: "本地目录", Width: 180},
							{Title: "轮询（秒）", Width: 80},
							{Title: "状态", Width: 100},
						},
					},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							PushButton{AssignTo: &aw.addHostButton, Text: "添加主机…", OnClicked: aw.addHost},
							PushButton{AssignTo: &aw.editHostButton, Text: "编辑…", OnClicked: aw.editSelectedHost},
							PushButton{AssignTo: &aw.removeHostButton, Text: "删除", OnClicked: aw.removeSelectedHost},
							PushButton{AssignTo: &aw.startHostButton, Text: "开始同步", OnClicked: aw.startSelectedHost},
							PushButton{AssignTo: &aw.stopHostButton, Text: "停止", OnClicked: aw.stopSelectedHost},
							PushButton{AssignTo: &aw.startAllButton, Text: "全部开始", OnClicked: aw.startAllHosts},
							PushButton{AssignTo: &aw.stopAllButton, Text: "全部停止", OnClicked: aw.stopAllHosts},
						},
					},
				},
			},
			Label{AssignTo: &aw.statusLabel, Text: "状态：就绪"},
			Label{Text: "同步规则：仅处理远程根目录下各子文件夹中的常规文件；根目录下的脚本、配置和其他散文件会被跳过。仅同步 UTC 修改时间早于当前时间 30 分钟的文件，下载后保留目录结构并删除远程源文件。"},
			VSplitter{
				StretchFactor: 1,
				Children: []Widget{
					Composite{
						Layout: VBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "当前任务"},
							TableView{
								AssignTo:            &aw.taskView,
								Model:               aw.taskModel,
								LastColumnStretched: true,
								Columns: []TableViewColumn{
									{Title: "主机", Width: 110},
									{Title: "状态", Width: 60},
									{Title: "路径", Width: 330},
									{Title: "大小", Width: 80},
									{Title: "进度", Width: 170},
									{Title: "速度", Width: 90},
								},
							},
						},
					},
					Composite{
						Layout: VBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "日志"},
							LogView{AssignTo: &aw.logView},
						},
					},
				},
			},
		},
		OnSizeChanged: aw.handleSizeChanged,
	}).Create()
}

func (aw *appWindow) initTray() error {
	tray, err := walk.NewNotifyIcon(aw.MainWindow)
	if err != nil {
		return err
	}
	aw.tray = tray

	if icon, err := loadAppIcon(); err == nil && icon != nil {
		_ = aw.SetIcon(icon)
		_ = tray.SetIcon(icon)
	}

	if err := tray.SetToolTip("SFTP Syncer"); err != nil {
		return err
	}
	if err := tray.SetVisible(true); err != nil {
		return err
	}

	tray.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			aw.showFromTray()
		}
	})

	showAction := walk.NewAction()
	_ = showAction.SetText("显示主窗口")
	showAction.Triggered().Attach(func() {
		aw.showFromTray()
	})
	tray.ContextMenu().Actions().Add(showAction)

	exitAction := walk.NewAction()
	_ = exitAction.SetText("退出")
	exitAction.Triggered().Attach(func() {
		aw.exitApp()
	})
	tray.ContextMenu().Actions().Add(exitAction)

	return nil
}

func loadAppIcon() (*walk.Icon, error) {
	return walk.NewIconFromSysDLL("shell32", 44)
}

func (aw *appWindow) bindEvents() {
	aw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if aw.exiting {
			return
		}
		*canceled = true
		aw.hideToTray(false)
	})
	aw.hostView.SelectedIndexesChanged().Attach(func() {
		aw.updateHostButtons()
	})
	aw.hostView.ItemActivated().Attach(func() {
		aw.editSelectedHost()
	})
}

func (aw *appWindow) handleSizeChanged() {
	if aw.MainWindow == nil || aw.IsDisposed() || aw.exiting {
		return
	}
	if !aw.allowMinimizeToTray {
		return
	}
	if win.IsIconic(aw.Handle()) {
		aw.hideToTray(true)
	}
}

func (aw *appWindow) hideToTray(showBalloon bool) {
	aw.Hide()
	if showBalloon && !aw.trayHintShown && aw.tray != nil {
		aw.trayHintShown = true
		_ = aw.tray.ShowInfo("SFTP Syncer", "程序已最小化到系统托盘，左键图标可恢复窗口，右键可退出。")
	}
}

func (aw *appWindow) showFromTray() {
	if aw.MainWindow == nil || aw.IsDisposed() {
		return
	}
	aw.Show()
	win.ShowWindow(aw.Handle(), win.SW_RESTORE)
	_ = aw.BringToTop()
	_ = aw.SetFocus()
}

// loadHosts loads the persisted host list (migrating the legacy single-host
// config on first launch) and populates the host table.
func (aw *appWindow) loadHosts() {
	store, err := config.LoadStore()
	if err != nil {
		aw.appendLogLevel(logLevelError, fmt.Sprintf("加载主机列表失败：%v", err))
	}
	if store == nil {
		store = &config.Store{}
	}
	aw.store = store
	aw.reloadHostTable()

	if path, pathErr := config.StorePath(); pathErr == nil {
		aw.appendLog(fmt.Sprintf("主机列表文件路径：%s", path))
	}
	if len(store.Hosts) == 0 {
		aw.appendLog("尚未添加主机。点击“添加主机…”开始配置。")
	}
}

// reloadHostTable refreshes the host table from the store, preserving the
// running status of active hosts.
func (aw *appWindow) reloadHostTable() {
	aw.syncMu.Lock()
	running := make(map[string]bool, len(aw.runs))
	for id := range aw.runs {
		running[id] = true
	}
	aw.syncMu.Unlock()

	cfgs := append([]config.Config(nil), aw.store.Hosts...)
	aw.hostModel.setHosts(cfgs, running)
	aw.hostModel.PublishRowsReset()
	// Row indexes changed: drop any stale selection so buttons reflect reality.
	_ = aw.hostView.SetSelectedIndexes(nil)
	aw.updateHostButtons()
}

// selectedHostID returns the host ID of the currently selected table row.
func (aw *appWindow) selectedHostID() (string, bool) {
	if aw.hostView == nil {
		return "", false
	}
	indexes := aw.hostView.SelectedIndexes()
	if len(indexes) == 0 {
		return "", false
	}
	return aw.hostModel.idAt(indexes[0])
}

func (aw *appWindow) isRunning(id string) bool {
	aw.syncMu.Lock()
	defer aw.syncMu.Unlock()
	_, running := aw.runs[id]
	return running
}

func (aw *appWindow) runningCount() int {
	aw.syncMu.Lock()
	defer aw.syncMu.Unlock()
	return len(aw.runs)
}

// updateHostButtons enables the host toolbar buttons according to the
// current selection and run states. Must be called on the UI thread.
func (aw *appWindow) updateHostButtons() {
	if aw.exiting || aw.MainWindow == nil || aw.IsDisposed() || aw.store == nil {
		return
	}
	id, ok := aw.selectedHostID()
	running := ok && aw.isRunning(id)

	if aw.editHostButton != nil {
		aw.editHostButton.SetEnabled(ok && !running)
	}
	if aw.removeHostButton != nil {
		aw.removeHostButton.SetEnabled(ok && !running)
	}
	if aw.startHostButton != nil {
		aw.startHostButton.SetEnabled(ok && !running)
	}
	if aw.stopHostButton != nil {
		aw.stopHostButton.SetEnabled(ok && running)
	}
	if aw.startAllButton != nil {
		aw.startAllButton.SetEnabled(len(aw.store.Hosts) > 0)
	}
	if aw.stopAllButton != nil {
		aw.stopAllButton.SetEnabled(aw.runningCount() > 0)
	}
}

// updateOverallStatus refreshes the status label and tray tooltip. Must be
// called on the UI thread.
func (aw *appWindow) updateOverallStatus() {
	if aw.MainWindow == nil || aw.IsDisposed() {
		return
	}
	running := aw.runningCount()
	text := "状态：就绪"
	if running > 0 {
		text = fmt.Sprintf("状态：运行中（%d/%d 台主机）", running, len(aw.store.Hosts))
	}
	_ = aw.statusLabel.SetText(text)
	if aw.tray != nil {
		if running > 0 {
			_ = aw.tray.SetToolTip(fmt.Sprintf("SFTP Syncer（%d 台主机同步中）", running))
		} else {
			_ = aw.tray.SetToolTip("SFTP Syncer")
		}
	}
}

// setHostStatus updates one host's run status and refreshes dependent UI.
func (aw *appWindow) setHostStatus(id, status string) {
	aw.hostModel.setStatus(id, status)
	if aw.MainWindow == nil || aw.IsDisposed() {
		return
	}
	aw.Synchronize(func() {
		if aw.MainWindow == nil || aw.IsDisposed() {
			return
		}
		if idx := aw.hostModel.rowOf(id); idx >= 0 {
			aw.hostModel.PublishRowChanged(idx)
		}
		aw.updateHostButtons()
		aw.updateOverallStatus()
	})
}

// persistStore saves the host list to disk, reporting failures to the user.
func (aw *appWindow) persistStore() {
	if err := aw.store.Save(); err != nil {
		walk.MsgBox(aw.MainWindow, "保存失败", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
	}
}

func (aw *appWindow) addHost() {
	cfg, accepted := runHostDialog(aw, "添加主机", config.Default())
	if !accepted {
		return
	}
	added := aw.store.Add(cfg)
	aw.persistStore()
	aw.reloadHostTable()
	if idx, ok := aw.hostModel.indexOf(added.ID); ok {
		_ = aw.hostView.SetSelectedIndexes([]int{idx})
	}
	aw.appendLog(fmt.Sprintf("已添加主机：%s（%s:%d）。", added.DisplayName(), added.Host, added.Port))
}

func (aw *appWindow) editSelectedHost() {
	id, ok := aw.selectedHostID()
	if !ok {
		return
	}
	if aw.isRunning(id) {
		walk.MsgBox(aw.MainWindow, "主机正在运行", "请先停止该主机的同步任务，再编辑其配置。", walk.MsgBoxOK|walk.MsgBoxIconWarning)
		return
	}
	cfg, found := aw.store.Find(id)
	if !found {
		return
	}
	updated, accepted := runHostDialog(aw, "编辑主机", cfg)
	if !accepted {
		return
	}
	updated.ID = cfg.ID
	if !aw.store.Update(updated) {
		return
	}
	aw.persistStore()
	aw.reloadHostTable()
	aw.appendLog(fmt.Sprintf("已更新主机：%s。", updated.DisplayName()))
}

func (aw *appWindow) removeSelectedHost() {
	id, ok := aw.selectedHostID()
	if !ok {
		return
	}
	if aw.isRunning(id) {
		walk.MsgBox(aw.MainWindow, "主机正在运行", "请先停止该主机的同步任务，再删除。", walk.MsgBoxOK|walk.MsgBoxIconWarning)
		return
	}
	cfg, found := aw.store.Find(id)
	if !found {
		return
	}
	if walk.MsgBox(aw.MainWindow, "确认删除",
		fmt.Sprintf("确定删除主机“%s”吗？", cfg.DisplayName()),
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	aw.store.Remove(id)
	aw.persistStore()
	aw.taskModel.clearHost(id)
	aw.publishTaskReset()
	aw.reloadHostTable()
	aw.appendLog(fmt.Sprintf("已删除主机：%s。", cfg.DisplayName()))
}

func (aw *appWindow) startSelectedHost() {
	id, ok := aw.selectedHostID()
	if !ok {
		return
	}
	aw.startHost(id)
}

func (aw *appWindow) stopSelectedHost() {
	id, ok := aw.selectedHostID()
	if !ok {
		return
	}
	aw.stopHost(id)
}

func (aw *appWindow) startAllHosts() {
	ids := make([]string, 0, len(aw.store.Hosts))
	for _, cfg := range aw.store.Hosts {
		ids = append(ids, cfg.ID)
	}
	for _, id := range ids {
		aw.startHost(id)
	}
}

func (aw *appWindow) stopAllHosts() {
	aw.stopAllRuns()
}

// startHost launches the sync loop for one host. It is a no-op when the host
// is unknown or already running. Called on the UI thread.
func (aw *appWindow) startHost(id string) {
	if aw.exiting {
		return
	}
	cfg, ok := aw.store.Find(id)
	if !ok {
		return
	}

	aw.syncMu.Lock()
	if _, running := aw.runs[id]; running {
		aw.syncMu.Unlock()
		return
	}
	label := cfg.DisplayName()
	ctx, cancel := context.WithCancel(context.Background())
	service := syncer.NewServiceLevel(func(level syncer.Level, format string, args ...any) {
		aw.appendLogLevel(logLevelFromSyncer(level), fmt.Sprintf("[%s] %s", label, fmt.Sprintf(format, args...)))
	})
	service.SetHost(cfg.ID, label)
	service.SetProgressCallback(aw.handleProgress)
	aw.runs[id] = &hostRun{cancel: cancel}
	aw.syncMu.Unlock()

	aw.setHostStatus(id, hostStatusRunning)
	aw.appendLog(fmt.Sprintf("[%s] 开始同步：服务器=%s:%d，远程目录=%s，本地目录=%s，轮询间隔=%d 秒。",
		label, cfg.Host, cfg.Port, cfg.RemoteDir, cfg.LocalDir, cfg.PollIntervalSeconds))
	go aw.runHostLoop(ctx, cfg, service)
}

func (aw *appWindow) stopHost(id string) {
	aw.syncMu.Lock()
	run, running := aw.runs[id]
	aw.syncMu.Unlock()
	if !running {
		return
	}
	aw.appendLog(fmt.Sprintf("[%s] 正在停止同步循环 ...", aw.hostModel.displayNameOf(id)))
	run.cancel()
	aw.setHostStatus(id, hostStatusStopping)
}

func (aw *appWindow) stopAllRuns() {
	aw.syncMu.Lock()
	ids := make([]string, 0, len(aw.runs))
	for id, run := range aw.runs {
		ids = append(ids, id)
		run.cancel()
	}
	aw.syncMu.Unlock()
	for _, id := range ids {
		aw.setHostStatus(id, hostStatusStopping)
	}
}

// runHostLoop is the per-host polling loop; one goroutine per running host.
func (aw *appWindow) runHostLoop(ctx context.Context, cfg config.Config, service *syncer.Service) {
	label := cfg.DisplayName()
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second

	defer func() {
		aw.syncMu.Lock()
		delete(aw.runs, cfg.ID)
		aw.syncMu.Unlock()

		aw.taskModel.clearHost(cfg.ID)
		aw.publishTaskReset()
		aw.setHostStatus(cfg.ID, hostStatusStopped)
		if !aw.exiting {
			aw.appendLog(fmt.Sprintf("[%s] 同步循环已停止。", label))
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		aw.taskModel.clearHost(cfg.ID)
		aw.publishTaskReset()

		result, err := service.RunOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			aw.appendLogLevel(logLevelError, fmt.Sprintf("[%s] 本轮同步失败：%v", label, err))
			aw.setHostStatus(cfg.ID, hostStatusError)
		} else {
			aw.appendLog(fmt.Sprintf("[%s] 本轮完成：扫描 %d 个文件，下载 %d，删除远程文件 %d，跳过根目录散文件 %d，跳过最近 30 分钟 %d，跳过非常规条目 %d，跳过隐藏目录 %d，本地已存在并直接删远程 %d。",
				label, result.Scanned, result.Downloaded, result.Deleted, result.SkippedRootFiles, result.SkippedRecent, result.SkippedNonRegular, result.SkippedHiddenDirs, result.AlreadyPresent))
			aw.setHostStatus(cfg.ID, hostStatusRunning)
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

func (aw *appWindow) exitApp() {
	aw.exiting = true
	aw.stopAllRuns()
	aw.stopTaskRefresh()
	_ = aw.Close()
}

func (aw *appWindow) handleProgress(evt syncer.ProgressEvent) {
	aw.taskModel.updateEntry(evt)
}

func (aw *appWindow) publishTaskReset() {
	if aw.MainWindow == nil || aw.IsDisposed() {
		return
	}
	aw.Synchronize(func() {
		if aw.MainWindow == nil || aw.IsDisposed() {
			return
		}
		aw.taskModel.PublishRowsReset()
	})
}

// startTaskRefresh launches a goroutine that repaints the task table every
// 500 ms while the window is alive. PublishRowsReset uses
// LVSICF_NOINVALIDATEALL, so it won't repaint existing rows when the count is
// unchanged. We must call taskView.Invalidate() which calls InvalidateRect on
// the internal ListView HWNDs and triggers LVN_GETDISPINFO to pull fresh data
// from the model for every visible row.
func (aw *appWindow) startTaskRefresh() {
	stop := make(chan struct{})
	aw.taskRefreshStop = stop
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if aw.MainWindow == nil || aw.IsDisposed() {
					return
				}
				aw.Synchronize(func() {
					if aw.MainWindow == nil || aw.IsDisposed() {
						return
					}
					aw.taskModel.PublishRowsReset()
					_ = aw.taskView.Invalidate()
				})
			}
		}
	}()
}

func (aw *appWindow) stopTaskRefresh() {
	if aw.taskRefreshStop != nil {
		close(aw.taskRefreshStop)
		aw.taskRefreshStop = nil
	}
}

// appendLog appends an INFO-level message; see appendLogLevel.
func (aw *appWindow) appendLog(message string) {
	aw.appendLogLevel(logLevelInfo, message)
}

// appendLogLevel appends one timestamped, severity-coloured line to the log
// view. It is safe to call from any goroutine.
func (aw *appWindow) appendLogLevel(level logLevel, message string) {
	if aw.MainWindow == nil || aw.IsDisposed() || aw.logView == nil {
		return
	}

	line := logLine{
		level: level,
		text:  fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), strings.TrimSpace(message)),
	}
	aw.Synchronize(func() {
		if aw.MainWindow == nil || aw.IsDisposed() || aw.logView == nil {
			return
		}
		aw.logLines = append(aw.logLines, line)
		if len(aw.logLines) > maxLogLines {
			aw.logLines = append([]logLine(nil), aw.logLines[len(aw.logLines)-maxLogLines:]...)
			aw.logView.replaceLines(aw.logLines)
		} else {
			aw.logView.appendLines(aw.logLines[len(aw.logLines)-1:])
		}
	})
}

// hostRow is one row in the host table: settings plus current run status.
type hostRow struct {
	cfg    config.Config
	status string
}

// hostTableModel is a walk TableModel that lists the managed sync hosts.
type hostTableModel struct {
	walk.TableModelBase
	mu    sync.Mutex
	items []*hostRow
	index map[string]int // hostID → items index
}

func newHostTableModel() *hostTableModel {
	return &hostTableModel{index: make(map[string]int)}
}

func (m *hostTableModel) RowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

func (m *hostTableModel) Value(row, col int) interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row < 0 || row >= len(m.items) {
		return ""
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.cfg.DisplayName()
	case 1:
		return fmt.Sprintf("%s:%d", item.cfg.Host, item.cfg.Port)
	case 2:
		return item.cfg.RemoteDir
	case 3:
		return item.cfg.LocalDir
	case 4:
		return fmt.Sprintf("%d", item.cfg.PollIntervalSeconds)
	case 5:
		return item.status
	}
	return ""
}

// setHosts replaces the table contents, deriving run statuses from running.
func (m *hostTableModel) setHosts(cfgs []config.Config, running map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = make([]*hostRow, len(cfgs))
	m.index = make(map[string]int, len(cfgs))
	for i := range cfgs {
		status := hostStatusStopped
		if running[cfgs[i].ID] {
			status = hostStatusRunning
		}
		m.items[i] = &hostRow{cfg: cfgs[i], status: status}
		m.index[cfgs[i].ID] = i
	}
}

// setStatus updates the run status of one host. It reports whether the host
// was found.
func (m *hostTableModel) setStatus(id, status string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.index[id]
	if !ok {
		return false
	}
	m.items[idx].status = status
	return true
}

// rowOf returns the current row index of a host, or -1 when unknown.
func (m *hostTableModel) rowOf(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx, ok := m.index[id]; ok {
		return idx
	}
	return -1
}

func (m *hostTableModel) indexOf(id string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx, ok := m.index[id]
	return idx, ok
}

// cfgAt returns the configuration of the row at the given index.
func (m *hostTableModel) cfgAt(row int) (config.Config, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row < 0 || row >= len(m.items) {
		return config.Config{}, false
	}
	return m.items[row].cfg, true
}

// idAt returns the host ID of the row at the given index.
func (m *hostTableModel) idAt(row int) (string, bool) {
	cfg, ok := m.cfgAt(row)
	return cfg.ID, ok
}

// displayNameOf returns the label of a host by ID, falling back to the ID.
func (m *hostTableModel) displayNameOf(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx, ok := m.index[id]; ok {
		return m.items[idx].cfg.DisplayName()
	}
	return id
}

// fileTaskEntry holds the current download state for a single remote file.
type fileTaskEntry struct {
	hostID     string
	hostLabel  string
	remotePath string
	totalBytes int64
	downloaded int64
	speedBps   float64
	state      string
}

// taskKey uniquely identifies a task row across hosts.
func taskKey(hostID, remotePath string) string {
	return hostID + "\x00" + remotePath
}

// fileTaskModel is a walk TableModel that shows per-file download progress
// across all hosts.
type fileTaskModel struct {
	walk.TableModelBase
	sortChangedPublisher walk.EventPublisher
	mu                   sync.Mutex
	items                []*fileTaskEntry
	index                map[string]int // taskKey → items index
	sortCol              int
	sortOrder            walk.SortOrder
}

func newFileTaskModel() *fileTaskModel {
	return &fileTaskModel{index: make(map[string]int), sortCol: -1}
}

func (m *fileTaskModel) RowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

func (m *fileTaskModel) Value(row, col int) interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row < 0 || row >= len(m.items) {
		return ""
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.hostLabel
	case 1:
		return item.state
	case 2:
		return item.remotePath
	case 3:
		return formatFileSize(item.totalBytes)
	case 4:
		if item.totalBytes <= 0 {
			return "—"
		}
		pct := int64(100) * item.downloaded / item.totalBytes
		return fmt.Sprintf("%s / %s (%d%%)", formatFileSize(item.downloaded), formatFileSize(item.totalBytes), pct)
	case 5:
		if item.speedBps < 1 {
			return "—"
		}
		return formatFileSize(int64(item.speedBps)) + "/s"
	}
	return ""
}

func (m *fileTaskModel) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = nil
	m.index = make(map[string]int)
}

// clearHost removes all task rows of the given host. It reports whether any
// rows were removed.
func (m *fileTaskModel) clearHost(hostID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.items[:0]
	removed := false
	for _, item := range m.items {
		if item.hostID == hostID {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	m.items = kept
	if removed {
		m.rebuildIndex()
	}
	return removed
}

func (m *fileTaskModel) ColumnSortable(_ int) bool { return true }

func (m *fileTaskModel) SortChanged() *walk.Event { return m.sortChangedPublisher.Event() }

func (m *fileTaskModel) SortedColumn() int { return m.sortCol }

func (m *fileTaskModel) SortOrder() walk.SortOrder { return m.sortOrder }

func (m *fileTaskModel) Sort(col int, order walk.SortOrder) error {
	m.mu.Lock()
	m.sortCol = col
	m.sortOrder = order
	m.sortItems()
	m.rebuildIndex()
	m.mu.Unlock()
	m.sortChangedPublisher.Publish()
	return nil
}

// sortItems re-sorts m.items in place. Must be called with m.mu held.
func (m *fileTaskModel) sortItems() {
	if m.sortCol < 0 || len(m.items) == 0 {
		return
	}
	col := m.sortCol
	asc := m.sortOrder == walk.SortAscending
	sort.SliceStable(m.items, func(i, j int) bool {
		a, b := m.items[i], m.items[j]
		var less bool
		switch col {
		case 0:
			less = a.hostLabel < b.hostLabel
		case 1:
			less = a.state < b.state
		case 2:
			less = a.remotePath < b.remotePath
		case 3:
			less = a.totalBytes < b.totalBytes
		case 4:
			var pa, pb int64
			if a.totalBytes > 0 {
				pa = 100 * a.downloaded / a.totalBytes
			}
			if b.totalBytes > 0 {
				pb = 100 * b.downloaded / b.totalBytes
			}
			less = pa < pb
		case 5:
			less = a.speedBps < b.speedBps
		}
		if asc {
			return less
		}
		return !less
	})
}

// rebuildIndex rebuilds the taskKey → index map. Must be called with m.mu held.
func (m *fileTaskModel) rebuildIndex() {
	m.index = make(map[string]int, len(m.items))
	for i, item := range m.items {
		m.index[taskKey(item.hostID, item.remotePath)] = i
	}
}

// updateEntry updates an existing entry or inserts a new one.
// Completed (Done) entries are removed from the list.
// Returns the row index of the changed row, or -1 if a full reset is needed.
func (m *fileTaskModel) updateEntry(evt syncer.ProgressEvent) int {
	stateStr := progressStateLabel(evt.State)
	key := taskKey(evt.HostID, evt.RemotePath)
	m.mu.Lock()
	defer m.mu.Unlock()

	if idx, ok := m.index[key]; ok {
		if evt.State == syncer.ProgressStateDone {
			// Remove completed task from the list.
			m.items = append(m.items[:idx], m.items[idx+1:]...)
			delete(m.index, key)
			for k, i := range m.index {
				if i > idx {
					m.index[k] = i - 1
				}
			}
			return -1
		}
		item := m.items[idx]
		item.state = stateStr
		if evt.Downloaded > 0 {
			item.downloaded = evt.Downloaded
		}
		if evt.Total > 0 {
			item.totalBytes = evt.Total
		}
		if evt.SpeedBps > 0 {
			item.speedBps = evt.SpeedBps
		}
		if evt.State == syncer.ProgressStateFailed {
			item.speedBps = 0
		}
		if m.sortCol >= 0 {
			m.sortItems()
			m.rebuildIndex()
			return -1
		}
		return idx
	}

	// Ignore Done events for entries not in the list.
	if evt.State == syncer.ProgressStateDone {
		return -1
	}

	entry := &fileTaskEntry{
		hostID:     evt.HostID,
		hostLabel:  evt.HostLabel,
		remotePath: evt.RemotePath,
		totalBytes: evt.Total,
		downloaded: evt.Downloaded,
		speedBps:   evt.SpeedBps,
		state:      stateStr,
	}
	m.items = append(m.items, entry)
	m.index[key] = len(m.items) - 1
	if m.sortCol >= 0 {
		m.sortItems()
		m.rebuildIndex()
	}
	return -1
}

func progressStateLabel(state syncer.ProgressState) string {
	switch state {
	case syncer.ProgressStatePending:
		return "待执行"
	case syncer.ProgressStateActive:
		return "下载中"
	case syncer.ProgressStateDone:
		return "已完成"
	case syncer.ProgressStateFailed:
		return "失败"
	}
	return string(state)
}

func formatFileSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	case bytes < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB", float64(bytes)/(1024*1024*1024))
	}
}

// hostEditDialog collects host settings in a modal dialog. The former
// main-window connection form now lives here.
type hostEditDialog struct {
	dlg   *walk.Dialog
	title string

	acceptButton *walk.PushButton
	cancelButton *walk.PushButton

	nameEdit                 *walk.LineEdit
	hostEdit                 *walk.LineEdit
	portEdit                 *walk.NumberEdit
	usernameEdit             *walk.LineEdit
	passwordAuthRadio        *walk.RadioButton
	privateKeyAuthRadio      *walk.RadioButton
	passwordEdit             *walk.LineEdit
	privateKeyPathEdit       *walk.LineEdit
	privateKeyPassphraseEdit *walk.LineEdit
	remoteDirEdit            *walk.LineEdit
	localDirEdit             *walk.LineEdit
	pollIntervalEdit         *walk.NumberEdit
	skipHostKeyCheck         *walk.CheckBox
	knownHostsPathEdit       *walk.LineEdit

	result config.Config
}

// runHostDialog shows the host editor and returns the edited configuration
// plus whether the user accepted the dialog.
func runHostDialog(owner walk.Form, title string, initial config.Config) (config.Config, bool) {
	d := &hostEditDialog{title: title}
	if err := d.build(owner); err != nil {
		walk.MsgBox(nil, "打开主机编辑窗口失败", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return initial, false
	}
	d.applyInitial(initial)

	if cmd := d.dlg.Run(); cmd != walk.DlgCmdOK {
		return initial, false
	}
	return d.result, true
}

func (d *hostEditDialog) build(owner walk.Form) error {
	return (Dialog{
		AssignTo:      &d.dlg,
		Title:         d.title,
		MinSize:       Size{Width: 560, Height: 600},
		DefaultButton: &d.acceptButton,
		CancelButton:  &d.cancelButton,
		Layout:        VBox{},
		Children: []Widget{
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "名称（可选）"},
					LineEdit{AssignTo: &d.nameEdit},

					Label{Text: "服务器地址"},
					LineEdit{AssignTo: &d.hostEdit},

					Label{Text: "端口"},
					NumberEdit{AssignTo: &d.portEdit, MinValue: 1, MaxValue: 65535, Value: 22.0, SpinButtonsVisible: true},

					Label{Text: "用户名"},
					LineEdit{AssignTo: &d.usernameEdit},
				},
			},
			GroupBox{
				Title:  "认证方式",
				Layout: VBox{},
				Children: []Widget{
					Composite{
						Layout: HBox{},
						Children: []Widget{
							RadioButton{AssignTo: &d.passwordAuthRadio, Text: "密码认证", OnClicked: d.updateAuthModeUI},
							RadioButton{AssignTo: &d.privateKeyAuthRadio, Text: "私钥认证", OnClicked: d.updateAuthModeUI},
						},
					},
					Composite{
						Layout: Grid{Columns: 2},
						Children: []Widget{
							Label{Text: "密码"},
							LineEdit{AssignTo: &d.passwordEdit, PasswordMode: true},

							Label{Text: "私钥文件路径"},
							LineEdit{AssignTo: &d.privateKeyPathEdit},

							Label{Text: "私钥口令（可选）"},
							LineEdit{AssignTo: &d.privateKeyPassphraseEdit, PasswordMode: true},
						},
					},
					Label{Text: "可选密码认证或私钥认证；若私钥已加密，可额外填写私钥口令。"},
				},
			},
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "远程目录"},
					LineEdit{AssignTo: &d.remoteDirEdit},

					Label{Text: "本地目录"},
					LineEdit{AssignTo: &d.localDirEdit},

					Label{Text: "轮询间隔（秒）"},
					NumberEdit{AssignTo: &d.pollIntervalEdit, MinValue: 5, MaxValue: 3600, Value: 30.0, SpinButtonsVisible: true},

					Label{Text: "主机密钥校验"},
					CheckBox{AssignTo: &d.skipHostKeyCheck, Text: "跳过主机密钥校验（连接更省事，但安全性较低）", Checked: true},

					Label{Text: "known_hosts 路径（可选）"},
					LineEdit{AssignTo: &d.knownHostsPathEdit},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{AssignTo: &d.acceptButton, Text: "确定", OnClicked: d.onAccept},
					PushButton{AssignTo: &d.cancelButton, Text: "取消", OnClicked: d.onCancel},
				},
			},
		},
	}).Create(owner)
}

func (d *hostEditDialog) applyInitial(cfg config.Config) {
	cfg = cfg.Normalized()
	_ = d.nameEdit.SetText(cfg.Name)
	_ = d.hostEdit.SetText(cfg.Host)
	_ = d.portEdit.SetValue(float64(cfg.Port))
	_ = d.usernameEdit.SetText(cfg.Username)
	d.setAuthMode(cfg.AuthMode)
	_ = d.passwordEdit.SetText(cfg.Password)
	_ = d.privateKeyPathEdit.SetText(cfg.PrivateKeyPath)
	_ = d.privateKeyPassphraseEdit.SetText(cfg.PrivateKeyPassphrase)
	_ = d.remoteDirEdit.SetText(cfg.RemoteDir)
	_ = d.localDirEdit.SetText(cfg.LocalDir)
	_ = d.pollIntervalEdit.SetValue(float64(cfg.PollIntervalSeconds))
	d.skipHostKeyCheck.SetChecked(cfg.SkipHostKeyValidation)
	_ = d.knownHostsPathEdit.SetText(cfg.KnownHostsPath)
	d.updateAuthModeUI()
}

func (d *hostEditDialog) onAccept() {
	cfg := d.collect()
	if err := cfg.Validate(); err != nil {
		walk.MsgBox(d.dlg, "配置无效", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}
	d.result = cfg
	d.dlg.Accept()
}

func (d *hostEditDialog) onCancel() {
	d.dlg.Cancel()
}

func (d *hostEditDialog) collect() config.Config {
	return config.Config{
		Name:                  d.nameEdit.Text(),
		Host:                  d.hostEdit.Text(),
		Port:                  int(math.Round(d.portEdit.Value())),
		Username:              d.usernameEdit.Text(),
		AuthMode:              d.selectedAuthMode(),
		Password:              d.passwordEdit.Text(),
		PrivateKeyPath:        d.privateKeyPathEdit.Text(),
		PrivateKeyPassphrase:  d.privateKeyPassphraseEdit.Text(),
		RemoteDir:             d.remoteDirEdit.Text(),
		LocalDir:              d.localDirEdit.Text(),
		PollIntervalSeconds:   int(math.Round(d.pollIntervalEdit.Value())),
		SkipHostKeyValidation: d.skipHostKeyCheck.Checked(),
		KnownHostsPath:        d.knownHostsPathEdit.Text(),
	}.Normalized()
}

func (d *hostEditDialog) selectedAuthMode() string {
	if d.privateKeyAuthRadio != nil && d.privateKeyAuthRadio.Checked() {
		return config.AuthModePrivateKey
	}
	return config.AuthModePassword
}

func (d *hostEditDialog) setAuthMode(mode string) {
	if mode == config.AuthModePrivateKey {
		if d.privateKeyAuthRadio != nil {
			d.privateKeyAuthRadio.SetChecked(true)
		}
		if d.passwordAuthRadio != nil {
			d.passwordAuthRadio.SetChecked(false)
		}
		return
	}

	if d.passwordAuthRadio != nil {
		d.passwordAuthRadio.SetChecked(true)
	}
	if d.privateKeyAuthRadio != nil {
		d.privateKeyAuthRadio.SetChecked(false)
	}
}

func (d *hostEditDialog) updateAuthModeUI() {
	passwordMode := d.selectedAuthMode() == config.AuthModePassword
	if d.passwordEdit != nil {
		d.passwordEdit.SetEnabled(passwordMode)
	}
	if d.privateKeyPathEdit != nil {
		d.privateKeyPathEdit.SetEnabled(!passwordMode)
	}
	if d.privateKeyPassphraseEdit != nil {
		d.privateKeyPassphraseEdit.SetEnabled(!passwordMode)
	}
}
