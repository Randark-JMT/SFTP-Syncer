package main

import (
	_ "embed"
	"fmt"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed build/windows/icon.ico
var trayIcon []byte

// RunTray 在独立 goroutine 中运行系统托盘：左键单击恢复窗口，
// 右键菜单提供「显示主窗口」「退出」。
func RunTray(a *App) {
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("SFTP Syncer")
		systray.SetOnTapped(func() {
			runtime.WindowShow(a.ctx)
		})

		mShow := systray.AddMenuItem("显示主窗口", "显示主窗口")
		mExit := systray.AddMenuItem("退出", "退出程序")

		go func() {
			for {
				select {
				case <-mShow.ClickedCh:
					runtime.WindowShow(a.ctx)
				case <-mExit.ClickedCh:
					a.Quit()
					return
				}
			}
		}()
	}, func() {
		// onExit：托盘消息循环结束，无需额外清理。
	})
}

// updateTrayTooltip 根据运行中的主机数量更新托盘提示。
func updateTrayTooltip(running, total int) {
	if running > 0 {
		systray.SetTooltip(fmt.Sprintf("SFTP Syncer（%d 台主机同步中）", running))
	} else {
		systray.SetTooltip("SFTP Syncer")
	}
}
