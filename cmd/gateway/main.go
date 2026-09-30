// Command gateway 是 ai-usage 的可执行入口。
//
// 子命令：
//
//	serve   启动 HTTP 网关（默认监听 127.0.0.1，可用 --listen 配置）
//	version 打印版本
//
// 无子命令时打印用法并退出（退出码 0）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// 各 provider adapter 通过 init() 自注册。聚合点必须在 cmd 入口
	// （provider 包不能 import 子 adapter，否则 import cycle）。
	_ "ai-usage/internal/provider/bailian"
	_ "ai-usage/internal/provider/deepseek"
	_ "ai-usage/internal/provider/kimicode"
	_ "ai-usage/internal/provider/mimo"
	_ "ai-usage/internal/provider/minimax"
	_ "ai-usage/internal/provider/modelscope"
	_ "ai-usage/internal/provider/moonshot"
	_ "ai-usage/internal/provider/openai"
	_ "ai-usage/internal/provider/opencodego"
	_ "ai-usage/internal/provider/zai"

	"ai-usage/internal/appconf"
	"ai-usage/internal/cache"
	"ai-usage/internal/config"
	"ai-usage/internal/server"
	"ai-usage/internal/store"
	"ai-usage/internal/web"
)

// version 可在编译时通过 -ldflags "-X main.version=..." 覆盖。
var version = "0.1.0"

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "命令行参数无效，请检查下列参数说明：")
		flag.PrintDefaults()
	}
	// 剥离子命令：重写 os.Args 为 [prog, ...子命令参数]，使 flag.Parse()
	// 从子命令参数开始解析（否则第一个非 flag 参数会阻断后续解析）。
	if len(os.Args) < 2 {
		usage(os.Stdout)
		return
	}
	sub := os.Args[1]
	rest := os.Args[2:]

	switch sub {
	case "serve":
		os.Args = append([]string{os.Args[0]}, rest...)
		runServe()
	case "version", "-v":
		fmt.Printf("ai-usage %s\n", version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q：请使用 help 查看命令说明\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// runServe 装配并启动 HTTP 网关。
//
// 装配顺序：config.Load() → store.Open(dataDir) → cache.New(ttl) →
// server.NewServer(st, cache, ttl) → web.NewHandler(s)。
// API 与 web 页面挂同一 mux（同源），保证浏览器写请求通过 Origin 校验。
func runServe() {
	appConfig, configPath, err := appconf.Load()
	if err != nil {
		log.Fatalf("配置加载失败：%s", oneLineError(err))
	}
	log.Printf("应用配置 %s", configPath)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置加载失败：%s", oneLineError(err))
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("数据目录打开失败：%s（请检查 --data-dir 路径与权限）", oneLineError(err))
	}
	defer st.Close()

	usageCache := cache.New(cfg.CacheTTL)
	srv := server.NewServer(st, usageCache, cfg.CacheTTL, appConfig)

	// 同源关键：API 与 web 页面挂同一 mux，浏览器写请求的 Origin
	// 与 Host 一致，通过 server.Wrap 的同源校验。
	// 认证在 Wrap 链上统一拦截：API 与网页同时受 Basic Auth 保护。
	mux := http.NewServeMux()
	mux.Handle("/api/", server.Wrap(srv.Handler(), cfg.Passwd))
	mux.Handle("/", server.Wrap(web.NewHandler(srv), cfg.Passwd))

	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.Port)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// 启动日志：不含任何 key 明文，也绝不含密码。
	listenDesc := "仅本机"
	if cfg.ListenHost != "127.0.0.1" && cfg.ListenHost != "localhost" {
		listenDesc = "对外可访问"
	}
	log.Printf("ai-usage %s 启动", version)
	log.Printf("监听 %s（%s）", addr, listenDesc)
	if cfg.Passwd != "" {
		log.Printf("密码认证已启用（Basic Auth，用户名 admin）")
	} else {
		log.Printf("无认证（仅本机建议）")
	}
	log.Printf("data-dir %s", cfg.DataDir)
	log.Printf("db %s", filepath.Join(cfg.DataDir, "gateway.db"))
	log.Printf("cache-ttl %s", cfg.CacheTTL)

	// 优雅关闭：SIGINT/SIGTERM → Shutdown（5s 超时）。
	shutdownDone := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Printf("收到退出信号，正在优雅关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("优雅关闭失败: %s", oneLineError(err))
		}
		close(shutdownDone)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 启动失败：%s（端口可能被占用，可换 --port）", oneLineError(err))
	}
	<-shutdownDone
	log.Printf("已退出")
}

func oneLineError(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}

// usage 打印子命令用法说明。无子命令时输出到 stdout（退出码 0），
// 未知子命令时输出到 stderr（退出码 2）。
func usage(w io.Writer) {
	fmt.Fprintf(w, `ai-usage %s — AI 用量助手（默认仅监听 127.0.0.1）

用法:
  %[2]s serve   [--port 8080] [--listen 127.0.0.1] [--passwd <密码>] [--data-dir <路径>] [--cache-ttl 5m]  启动 HTTP 网关
  %[2]s version                                                     打印版本

子命令详情:
  serve
    --port       监听端口（默认 8080，env: AI_USAGE_PORT）
    --listen     监听地址（默认 127.0.0.1；可填 0.0.0.0/局域网 IP 对外访问，env: AI_USAGE_LISTEN）
    --passwd     访问密码，Basic Auth 用户名 admin（默认空=无认证；密码不加密数据库，env: AI_USAGE_PASSWD）
    --data-dir   数据目录（默认 ~/.local/share/ai-usage，env: AI_USAGE_DATA_DIR）
    --cache-ttl  用量缓存有效期（默认 5m，如 5m/30s，env: AI_USAGE_CACHE_TTL）

`, version, os.Args[0])
}
