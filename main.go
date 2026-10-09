package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// main 是独立服务的入口（§11）：解析 flag、加载配置、建依赖、起 HTTP 服务、优雅关闭。
// 注意：flag.Parse() 只能写在这里，禁止放进 init()（否则 go test 会因 -test.* 直接 FAIL，§3.1）。
func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	// -version：打印 "<name> <version>" 后 exit 0。
	if *showVersion {
		fmt.Printf("zen2api %s\n", version)
		return
	}

	// 读配置 → 失败：中文原因到 stderr + os.Exit(1)。
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("读取配置文件 %q 失败: %w", *configPath, err))
		os.Exit(1)
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 建 logger → info 打印版本 + 生效配置 summary()。
	log, err := newLogger(cfg.Server.LogLevel, cfg.Server.LogFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log.info(fmt.Sprintf("zen2api %s 启动", version), nil)
	log.info("生效配置:\n"+cfg.summary(), nil)
	if len(cfg.Server.APIKeys) == 0 {
		log.warn("未配置 api_keys，/v1 不鉴权（仅建议本机监听时使用）", nil)
	}

	// 先建代理池（catalog_via_proxy=true 时要用它当目录出口）→ 失败 error 日志 + os.Exit(1)。
	pool, err := buildProxyPool(&cfg, log)
	if err != nil {
		log.error(err.Error(), nil)
		_ = log.Close()
		os.Exit(1)
	}

	// 加载目录缓存并首次 fetch：失败只 warn（不致命，用缓存兜底，§5.3）。
	cat := loadCatalog(cfg.Upstream.CatalogFile)
	var doer httpDoer // nil = 直连
	if cfg.catalogViaProxy() {
		doer = pool
	}
	if err := cat.fetch(context.Background(), cfg.baseURL(),
		cfg.Upstream.CatalogTimeout.value(defaultCatalogTimeout), doer); err != nil {
		log.warn("首次拉取上游目录失败，沿用缓存: "+err.Error(), nil)
	}

	// 建路由器，并同步暴露集合（目录命中 matchesUpstream 的 id ∪ 显式 models）。
	rt := newRouter(cfg.Upstream.ModelPrefix, cfg.Upstream.ModelAliases)
	rt.setExposed(exposedFromCatalog(&cfg, cat))

	startedAt := time.Now()
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           routes(&cfg, log, pool, rt, cat, startedAt),
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		// 监听失败（端口占用等）→ 中文 error 日志 + os.Exit(1)。
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.error("HTTP 服务启动失败: "+err.Error(), nil)
			_ = log.Close()
			os.Exit(1)
		}
	case <-ctx.Done():
		log.info("收到退出信号，开始优雅关闭", nil)
		shutdownCtx, cancel := context.WithTimeout(context.Background(),
			cfg.Server.ShutdownTimeout.value(defaultShutdownTimeout))
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.error("优雅关闭失败: "+err.Error(), nil)
		}
		cancel()
	}
	_ = log.Close()
}
