//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"sftp-syncer/internal/config"
	"sftp-syncer/internal/syncer"
)

type appWindow struct {
	*walk.MainWindow

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
	startButton              *walk.PushButton
	stopButton               *walk.PushButton
	saveButton               *walk.PushButton
	statusLabel              *walk.Label
	logView                  *walk.TextEdit
	tray                     *walk.NotifyIcon
	trayHintShown            bool
	logLines                 []string
	exiting                  bool
	allowMinimizeToTray      bool
	syncMu                   sync.Mutex
	syncCancel               context.CancelFunc
	syncRunning              bool
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

	window.loadPersistedConfig()
	window.Starting().Attach(func() {
		win.ShowWindow(window.Handle(), win.SW_SHOWNORMAL)
		window.appendLog("应用已启动。最小化或关闭窗口时会隐藏到系统托盘。")
		window.setStatus("状态：就绪")
		window.allowMinimizeToTray = true
	})

	window.Run()
}

func showStartupError(err error) {
	log.Printf("启动 GUI 失败: %v", err)
	walk.MsgBox(nil, "SFTP Syncer 启动失败", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func newAppWindow() (*appWindow, error) {
	aw := &appWindow{}
	if err := aw.buildUI(); err != nil {
		return nil, err
	}
	if err := aw.initTray(); err != nil {
		aw.Dispose()
		return nil, err
	}
	aw.bindEvents()
	aw.setAuthMode(config.AuthModePassword)
	aw.setControlsForRunning(false)
	return aw, nil
}

func (aw *appWindow) buildUI() error {
	return (MainWindow{
		AssignTo: &aw.MainWindow,
		Name:     "SFTPSyncerMainWindow",
		Title:    "SFTP Syncer",
		MinSize:  Size{Width: 820, Height: 760},
		Size:     Size{Width: 920, Height: 820},
		Layout:   VBox{},
		Children: []Widget{
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "服务器地址"},
					LineEdit{AssignTo: &aw.hostEdit},

					Label{Text: "端口"},
					NumberEdit{AssignTo: &aw.portEdit, MinValue: 1, MaxValue: 65535, Value: 22.0, SpinButtonsVisible: true},

					Label{Text: "用户名"},
					LineEdit{AssignTo: &aw.usernameEdit},
				},
			},
			GroupBox{
				Title:  "认证方式",
				Layout: VBox{},
				Children: []Widget{
					Composite{
						Layout: HBox{},
						Children: []Widget{
							RadioButton{AssignTo: &aw.passwordAuthRadio, Text: "密码认证", OnClicked: aw.updateAuthModeUI},
							RadioButton{AssignTo: &aw.privateKeyAuthRadio, Text: "私钥认证", OnClicked: aw.updateAuthModeUI},
						},
					},
					Composite{
						Layout: Grid{Columns: 2},
						Children: []Widget{
							Label{Text: "密码"},
							LineEdit{AssignTo: &aw.passwordEdit, PasswordMode: true},

							Label{Text: "私钥文件路径"},
							LineEdit{AssignTo: &aw.privateKeyPathEdit},

							Label{Text: "私钥口令（可选）"},
							LineEdit{AssignTo: &aw.privateKeyPassphraseEdit, PasswordMode: true},
						},
					},
					Label{Text: "可选密码认证或私钥认证；若私钥已加密，可额外填写私钥口令。"},
				},
			},
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{

					Label{Text: "远程目录"},
					LineEdit{AssignTo: &aw.remoteDirEdit},

					Label{Text: "本地目录"},
					LineEdit{AssignTo: &aw.localDirEdit},

					Label{Text: "轮询间隔（秒）"},
					NumberEdit{AssignTo: &aw.pollIntervalEdit, MinValue: 5, MaxValue: 3600, Value: 30.0, SpinButtonsVisible: true},

					Label{Text: "主机密钥校验"},
					CheckBox{AssignTo: &aw.skipHostKeyCheck, Text: "跳过主机密钥校验（连接更省事，但安全性较低）", Checked: true},

					Label{Text: "known_hosts 路径（可选）"},
					LineEdit{AssignTo: &aw.knownHostsPathEdit},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{AssignTo: &aw.startButton, Text: "开始同步", OnClicked: aw.startSync},
					PushButton{AssignTo: &aw.stopButton, Text: "停止", OnClicked: aw.stopSyncFromUI},
					PushButton{AssignTo: &aw.saveButton, Text: "保存配置", OnClicked: aw.saveConfigFromUI},
				},
			},
			Label{AssignTo: &aw.statusLabel, Text: "状态：就绪"},
			Label{Text: "同步规则：仅处理远程根目录下各子文件夹中的常规文件；根目录下的脚本、配置和其他散文件会被跳过。仅同步 UTC 修改时间早于当前时间 30 分钟的文件，下载后保留目录结构并删除远程源文件。"},
			TextEdit{AssignTo: &aw.logView, ReadOnly: true, VScroll: true, StretchFactor: 1},
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

func (aw *appWindow) loadPersistedConfig() {
	cfg, err := config.Load()
	if err != nil {
		aw.appendLog(fmt.Sprintf("加载配置失败：%v", err))
		return
	}
	aw.applyConfig(cfg)

	path, pathErr := config.ConfigPath()
	if pathErr == nil {
		aw.appendLog(fmt.Sprintf("配置文件路径：%s", path))
	}
}

func (aw *appWindow) applyConfig(cfg config.Config) {
	cfg = cfg.Normalized()
	_ = aw.hostEdit.SetText(cfg.Host)
	_ = aw.portEdit.SetValue(float64(cfg.Port))
	_ = aw.usernameEdit.SetText(cfg.Username)
	aw.setAuthMode(cfg.AuthMode)
	_ = aw.passwordEdit.SetText(cfg.Password)
	_ = aw.privateKeyPathEdit.SetText(cfg.PrivateKeyPath)
	_ = aw.privateKeyPassphraseEdit.SetText(cfg.PrivateKeyPassphrase)
	_ = aw.remoteDirEdit.SetText(cfg.RemoteDir)
	_ = aw.localDirEdit.SetText(cfg.LocalDir)
	_ = aw.pollIntervalEdit.SetValue(float64(cfg.PollIntervalSeconds))
	aw.skipHostKeyCheck.SetChecked(cfg.SkipHostKeyValidation)
	_ = aw.knownHostsPathEdit.SetText(cfg.KnownHostsPath)
	aw.updateAuthModeUI()
}

func (aw *appWindow) currentConfig() config.Config {
	return config.Config{
		Host:                  aw.hostEdit.Text(),
		Port:                  int(math.Round(aw.portEdit.Value())),
		Username:              aw.usernameEdit.Text(),
		AuthMode:              aw.selectedAuthMode(),
		Password:              aw.passwordEdit.Text(),
		PrivateKeyPath:        aw.privateKeyPathEdit.Text(),
		PrivateKeyPassphrase:  aw.privateKeyPassphraseEdit.Text(),
		RemoteDir:             aw.remoteDirEdit.Text(),
		LocalDir:              aw.localDirEdit.Text(),
		PollIntervalSeconds:   int(math.Round(aw.pollIntervalEdit.Value())),
		SkipHostKeyValidation: aw.skipHostKeyCheck.Checked(),
		KnownHostsPath:        aw.knownHostsPathEdit.Text(),
	}.Normalized()
}

func (aw *appWindow) selectedAuthMode() string {
	if aw.privateKeyAuthRadio != nil && aw.privateKeyAuthRadio.Checked() {
		return config.AuthModePrivateKey
	}
	return config.AuthModePassword
}

func (aw *appWindow) setAuthMode(mode string) {
	if mode == config.AuthModePrivateKey {
		if aw.privateKeyAuthRadio != nil {
			aw.privateKeyAuthRadio.SetChecked(true)
		}
		if aw.passwordAuthRadio != nil {
			aw.passwordAuthRadio.SetChecked(false)
		}
		return
	}

	if aw.passwordAuthRadio != nil {
		aw.passwordAuthRadio.SetChecked(true)
	}
	if aw.privateKeyAuthRadio != nil {
		aw.privateKeyAuthRadio.SetChecked(false)
	}
}

func (aw *appWindow) updateAuthModeUI() {
	editable := aw.saveButton == nil || aw.saveButton.Enabled()
	passwordMode := aw.selectedAuthMode() == config.AuthModePassword

	if aw.passwordEdit != nil {
		aw.passwordEdit.SetEnabled(editable && passwordMode)
	}
	if aw.privateKeyPathEdit != nil {
		aw.privateKeyPathEdit.SetEnabled(editable && !passwordMode)
	}
	if aw.privateKeyPassphraseEdit != nil {
		aw.privateKeyPassphraseEdit.SetEnabled(editable && !passwordMode)
	}
}

func (aw *appWindow) saveConfigFromUI() {
	cfg := aw.currentConfig()
	if err := cfg.Validate(); err != nil {
		walk.MsgBox(aw.MainWindow, "配置无效", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}
	if err := config.Save(cfg); err != nil {
		walk.MsgBox(aw.MainWindow, "保存失败", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}
	aw.appendLog("配置已保存。")
	walk.MsgBox(aw.MainWindow, "保存成功", "配置已保存。", walk.MsgBoxOK|walk.MsgBoxIconInformation)
}

func (aw *appWindow) startSync() {
	cfg := aw.currentConfig()
	if err := cfg.Validate(); err != nil {
		walk.MsgBox(aw.MainWindow, "配置无效", err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}
	if err := config.Save(cfg); err != nil {
		walk.MsgBox(aw.MainWindow, "保存失败", fmt.Sprintf("启动前保存配置失败：%v", err), walk.MsgBoxOK|walk.MsgBoxIconError)
		return
	}

	aw.syncMu.Lock()
	if aw.syncRunning {
		aw.syncMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	aw.syncCancel = cancel
	aw.syncRunning = true
	aw.syncMu.Unlock()

	aw.setControlsForRunning(true)
	aw.setStatus("状态：运行中")
	aw.appendLog(fmt.Sprintf("开始同步：服务器=%s:%d，远程目录=%s，本地目录=%s，轮询间隔=%d 秒。", cfg.Host, cfg.Port, cfg.RemoteDir, cfg.LocalDir, cfg.PollIntervalSeconds))

	go aw.runSyncLoop(ctx, cfg)
}

func (aw *appWindow) runSyncLoop(ctx context.Context, cfg config.Config) {
	service := syncer.NewService(func(format string, args ...any) {
		aw.appendLog(fmt.Sprintf(format, args...))
	})
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second

	defer func() {
		aw.syncMu.Lock()
		aw.syncRunning = false
		aw.syncCancel = nil
		aw.syncMu.Unlock()

		if aw.MainWindow != nil && !aw.IsDisposed() {
			aw.Synchronize(func() {
				aw.setControlsForRunning(false)
				if !aw.exiting {
					aw.setStatus("状态：已停止")
				}
			})
		}
	}()

	for {
		if ctx.Err() != nil {
			aw.appendLog("同步循环已停止。")
			return
		}

		result, err := service.RunOnce(ctx, cfg)
		if err != nil {
			if ctx.Err() != nil {
				aw.appendLog("同步循环已取消。")
				return
			}
			aw.appendLog(fmt.Sprintf("本轮同步失败：%v", err))
			aw.setStatus("状态：运行中（最近一轮失败）")
		} else {
			aw.appendLog(fmt.Sprintf("本轮完成：扫描 %d 个文件，下载 %d，删除远程文件 %d，跳过根目录散文件 %d，跳过最近 30 分钟 %d，跳过非常规条目 %d，本地已存在并直接删远程 %d。", result.Scanned, result.Downloaded, result.Deleted, result.SkippedRootFiles, result.SkippedRecent, result.SkippedNonRegular, result.AlreadyPresent))
			aw.setStatus("状态：运行中")
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			aw.appendLog("同步循环已停止。")
			return
		case <-timer.C:
		}
	}
}

func (aw *appWindow) stopSyncFromUI() {
	aw.stopSync(false)
}

func (aw *appWindow) stopSync(silent bool) {
	aw.syncMu.Lock()
	cancel := aw.syncCancel
	running := aw.syncRunning
	aw.syncMu.Unlock()

	if !running {
		return
	}
	if !silent {
		aw.appendLog("正在停止同步循环 ...")
		aw.setStatus("状态：正在停止")
	}
	if cancel != nil {
		cancel()
	}
}

func (aw *appWindow) exitApp() {
	aw.exiting = true
	aw.stopSync(true)
	_ = aw.Close()
}

func (aw *appWindow) setControlsForRunning(running bool) {
	widgets := []walk.Window{
		aw.hostEdit,
		aw.portEdit,
		aw.usernameEdit,
		aw.remoteDirEdit,
		aw.localDirEdit,
		aw.pollIntervalEdit,
		aw.skipHostKeyCheck,
		aw.knownHostsPathEdit,
		aw.saveButton,
		aw.passwordAuthRadio,
		aw.privateKeyAuthRadio,
	}
	for _, widget := range widgets {
		if widget != nil {
			widget.SetEnabled(!running)
		}
	}
	if aw.startButton != nil {
		aw.startButton.SetEnabled(!running)
	}
	if aw.stopButton != nil {
		aw.stopButton.SetEnabled(running)
	}
	aw.updateAuthModeUI()
}

func (aw *appWindow) setStatus(text string) {
	if aw.MainWindow == nil || aw.IsDisposed() || aw.statusLabel == nil {
		return
	}
	aw.Synchronize(func() {
		if aw.statusLabel != nil {
			_ = aw.statusLabel.SetText(text)
		}
	})
}

func (aw *appWindow) appendLog(message string) {
	if aw.MainWindow == nil || aw.IsDisposed() || aw.logView == nil {
		return
	}

	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), strings.TrimSpace(message))
	aw.Synchronize(func() {
		aw.logLines = append(aw.logLines, line)
		if len(aw.logLines) > 500 {
			aw.logLines = aw.logLines[len(aw.logLines)-500:]
		}
		_ = aw.logView.SetText(strings.Join(aw.logLines, "\r\n"))
		aw.logView.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
	})
}
