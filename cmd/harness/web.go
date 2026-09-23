package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go-ai-agent/internal/api"
	"go-ai-agent/internal/config"
)

// serveWeb 由 main() 调用；解析旗标后进入 Web 模式。
func serveWebCmd() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", `HTTP 监听地址（默认自动选择空闲端口）`)
	cfgPath := fs.String("config", "config/ai.yml", "配置文件路径")
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器")
	_ = fs.Parse(os.Args[1:]) // 未识别的参数会被忽略，保持零配置可用
	serveWeb(*addr, *cfgPath, *noBrowser)
}

// pickAddr 返回可绑定的监听地址：addr 非空直接用；为空则从内核获取随机空闲端口。
func pickAddr(addr string) (string, int, error) {
	if addr != "" {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return "", 0, fmt.Errorf("invalid -addr %q: %w", addr, err)
		}
		return addr, atoi(port), nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, fmt.Errorf("allocate free port: %w", err)
	}
	defer l.Close()
	return l.Addr().String(), l.Addr().(*net.TCPAddr).Port, nil
}

// atoi 宽松解析端口字符串（解析失败返回 0，仅用于展示）。
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// openBrowser 跨平台打开默认浏览器。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("(无法自动打开浏览器，请手动访问上述地址)")
	}
}

// serveWeb 启动 REST API + 内嵌前端；端口被占用时自动切换随机端口。
func serveWeb(addr, cfgPath string, noBrowser bool) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	srv, err := api.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	defer srv.Close()

	listenAddr, port, err := pickAddr(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	bound := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		bound <- err
	}()

	// 等待绑定结果：成功则打印地址并（可选）打开浏览器；失败（含端口占用）则回退随机端口。
	select {
	case err := <-bound:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 指定端口被占用 → 换随机端口重试一次
			fmt.Printf("port %d unavailable, picking a random port...\n", port)
			freeAddr, freePort, perr := pickAddr("")
			if perr != nil {
				fmt.Fprintln(os.Stderr, "harness:", perr)
				os.Exit(1)
			}
			httpSrv.Addr = freeAddr
			port = freePort
			bound = make(chan error, 1)
			go func() { bound <- httpSrv.ListenAndServe() }()
			if err := <-bound; err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "harness:", err)
				os.Exit(1)
			}
		}
	case <-time.After(500 * time.Millisecond):
		// 绑定成功且服务运行中
	}

	base := fmt.Sprintf("http://localhost:%d", port)
	fmt.Printf("harness %s listening on %s\n", version, base)
	fmt.Printf("web UI:     %s/\n", base)
	fmt.Printf("swagger UI: %s/swagger/index.html\n", base)
	fmt.Println("Ctrl+C 退出")
	if !noBrowser {
		openBrowser(base)
	}

	// 优雅停机
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
