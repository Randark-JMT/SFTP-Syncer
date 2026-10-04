package main

import (
	"context"
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "SFTP Syncer " + Version,
		Width:     1000,
		Height:    780,
		MinWidth:  860,
		MinHeight: 620,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			Theme:                windows.SystemDefault,
		},
		OnStartup: func(ctx context.Context) {
			app.SetContext(ctx)
			app.Startup()
			go RunTray(app)
		},
		OnBeforeClose: func(ctx context.Context) bool {
			if app.IsExiting() {
				return false
			}
			runtime.WindowHide(ctx)
			return true
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}
