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
	"strings"
	"syscall"
	"time"

	"go-ai-agent/internal/api"
	"go-ai-agent/internal/config"
)

// serveWebCmd 进入 Web 模式；args 为子命令后的剩余旗标（nil 表示无参数）。
func serveWebCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", `HTTP 监听地址（默认 127.0.0.1:8000，被占用则自动挑选空闲端口）`)
	cfgPath := fs.String("config", defaultConfigPath, "总配置文件路径")
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器")
	_ = fs.Parse(args) // 未识别的参数会被忽略，保持零配置可用
	serveWeb(*addr, *cfgPath, *noBrowser)
}

// defaultAddr 首选监听地址：本机回环 8000 端口。
// 默认只绑 127.0.0.1（不监听所有网卡）——这是本地单用户工具，暴露到局域网/公网
// 会让未鉴权的接口（默认无 api-tokens）直接可达。需要他人访问时，在
// config.yml 里显式写 server.addr: "0.0.0.0:8000" 或 ":8000"。
// 仅当该端口被占用时才回退随机空闲端口（同样只绑回环）。
const defaultAddr = "127.0.0.1:8000"

// reserveAddr 确定监听地址：addr 为空时取 defaultAddr，尝试绑定成功即返回；
// 若该端口已被占用，则回退到一个随机空闲端口并提示；其他错误（如权限不足）直接返回。
func reserveAddr(addr string) (string, int, error) {
	if addr == "" {
		addr = defaultAddr
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", 0, fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	port := pickPort(addr)
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
		return addr, port, nil
	}
	if !isAddrInUse(err) {
		return "", 0, fmt.Errorf("listen %s: %w", addr, err)
	}
	fmt.Printf("端口 %d 已被占用，改用随机空闲端口...\n", port)
	return pickAddr("")
}

// pickAddr 从内核获取一个随机空闲端口。
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

// pickPort 从监听地址串中取端口号（仅用于展示）。
func pickPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	return atoi(port)
}

// isAddrInUse 判断错误是否为「端口已被占用」。
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	// 兜底：Windows 的 WSAEADDRINUSE 未必能映射到 syscall.EADDRINUSE，
	// 按各平台/语言下的常见文案判断。
	msg := strings.ToLower(err.Error())
	for _, hint := range []string{
		"in use",
		"only one usage of each socket address",
		"只允许使用一次",
		"正在使用的地址",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
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
	applyLogLevel(cfg.Logging.Level)

	srv, err := api.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	srv.Version = version
	defer srv.Close()

	// 命令行旗标优先，其次 config.server.addr，两者都为空则用默认端口 8000
	if addr == "" {
		addr = cfg.Server.Addr
	}
	if noBrowser {
		// 显式关闭；否则跟随配置
	} else {
		noBrowser = !cfg.Server.OpenBrowser
	}

	// 首选 8000；仅当该端口已被占用时才回退随机空闲端口
	listenAddr, port, err := reserveAddr(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}

	readHeaderTimeout := time.Duration(cfg.Server.ReadHeaderTimeoutSeconds) * time.Second
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 10 * time.Second
	}
	shutdownTimeout := time.Duration(cfg.Server.ShutdownTimeoutSeconds) * time.Second
	if shutdownTimeout <= 0 {
		shutdownTimeout = 5 * time.Second
	}

	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: readHeaderTimeout,
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
	if cfg.Server.SwaggerOn() {
		fmt.Printf("swagger UI: %s/swagger/index.html\n", base)
	}
	fmt.Println("Ctrl+C 退出")
	if !noBrowser {
		openBrowser(base)
	}

	// 优雅停机
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
