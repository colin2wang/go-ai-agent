package desktop

import (
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"go-ai-agent/internal/api"
	"go-ai-agent/internal/config"
)

// runDesktop 启动 Wails 桌面应用：加载三层配置 → 初始化核心 → 打开主窗口。
func runDesktop(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	applyLogLevel(cfg.Logging.Level)

	srv, err := api.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	srv.Version = appVersion
	defer srv.Close()

	app := api.NewApp(srv)
	err = wails.Run(&options.App{
		Title:     cfg.App.Title,
		Width:     1360,
		Height:    860,
		MinWidth:  960,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: api.AssetsFS(),
		},
		OnStartup: app.OnStartup,
		Bind:      []interface{}{app},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
}
