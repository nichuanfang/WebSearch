// websearch 是多平台 AI 搜索网关的 MCP 服务器。
//
// 子命令：
//
//	websearch serve    启动 stdio MCP 服务（默认）
//	websearch quota    查看各平台额度状态
//	websearch config   查看生效配置（密钥脱敏）
//	websearch doctor   对各平台做连通性自检
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/fetch"
	"github.com/nichuanfang/WebSearch/internal/logging"
	"github.com/nichuanfang/WebSearch/internal/mcp"
	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/quota"
	"github.com/nichuanfang/WebSearch/internal/router"
	"github.com/nichuanfang/WebSearch/internal/search"
	"github.com/nichuanfang/WebSearch/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	var configPath string
	var passthrough []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return errors.New("--config 需要一个路径参数")
			}
			configPath = args[i+1]
			i++
		case "serve", "quota", "config", "doctor":
			cmd = args[i]
		default:
			passthrough = append(passthrough, args[i])
		}
	}
	_ = passthrough

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := logging.New(cfg.Server.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, configPath, log)
	case "quota":
		return showQuota(ctx, cfg, configPath)
	case "config":
		return showConfig(cfg, configPath)
	case "doctor":
		return doctor(ctx, cfg, configPath)
	default:
		return fmt.Errorf("未知子命令 %q", cmd)
	}
}

// build 组装各组件。
func build(cfg *config.Config, configPath string, log *slog.Logger) (*router.Router, *quota.Manager, *store.Store, error) {
	dbPath := cfg.ResolveDBPath(configPath)
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, nil, err
	}
	httpClient := &http.Client{Timeout: cfg.Server.RequestTimeout}
	qm := quota.NewManager(st, time.Now)

	var targets []router.Target
	for _, name := range []string{config.ProviderExa, config.ProviderBrave, config.ProviderTavily} {
		pc, ok := cfg.Providers[name]
		if !ok || !pc.Enabled {
			continue
		}
		key := cfg.ResolveKey(name)
		if key == "" {
			continue
		}
		var p provider.Provider
		switch name {
		case config.ProviderExa:
			p = provider.NewExa(key, httpClient)
		case config.ProviderBrave:
			p = provider.NewBrave(key, httpClient)
		case config.ProviderTavily:
			p = provider.NewTavily(key, httpClient)
		}
		targets = append(targets, router.Target{Provider: p, Config: pc, HasKey: true})
	}
	return router.New(targets, qm, st, log), qm, st, nil
}

// serve 启动 stdio MCP 服务。
func serve(ctx context.Context, cfg *config.Config, configPath string, log *slog.Logger) error {
	r, qm, st, err := build(cfg, configPath, log)
	if err != nil {
		return err
	}
	defer st.Close()

	// 未配置密钥的平台在启动日志中提示，避免运行时才发现。
	for _, name := range []string{config.ProviderExa, config.ProviderBrave, config.ProviderTavily} {
		if _, ok := r.TargetByName(name); !ok {
			log.Warn("平台未启用（缺少密钥或已禁用）", "provider", name)
		}
	}
	_ = qm

	svc := search.New(r, log, cfg.Routing.MaxAttempts)
	if cfg.Routing.Fallback.IsEnabled() {
		for _, name := range cfg.Routing.Fallback.SearchChain() {
			p := buildSearchFallback(name, cfg.Server.RequestTimeout)
			if p == nil {
				continue
			}
			// Bing 是最后退路：它失败后不再尝试其他通道。
			terminal := name == config.FallbackBing
			svc.WithFallbackOptions(p, cfg.Routing.Fallback.CooldownDuration(), terminal)
		}
		log.Info("免密钥搜索通道已启用",
			"chain", cfg.Routing.Fallback.SearchChain(),
			"cooldown", cfg.Routing.Fallback.CooldownDuration())
	}
	srv := mcp.NewServer(svc, buildFetcher(cfg, log), log)
	return mcp.Serve(ctx, srv, log)
}

// buildSearchFallback 按名称构造免密钥搜索通道。
func buildSearchFallback(name string, timeout time.Duration) provider.Provider {
	client := &http.Client{Timeout: timeout}
	switch name {
	case config.FallbackParallel:
		return provider.NewParallel(client)
	case config.FallbackBing:
		return provider.NewBing(client)
	default:
		return nil
	}
}

// buildFetcher 按配置组装抓取链：远程通道优先，本地抓取兜底。
func buildFetcher(cfg *config.Config, log *slog.Logger) *fetch.Manager {
	local := fetch.New(&http.Client{Timeout: cfg.Server.RequestTimeout}, fetch.DefaultMaxChars)
	if !cfg.Routing.Fallback.IsEnabled() {
		return fetch.NewManager(nil, local, 0, log)
	}
	var remote []fetch.RemoteFetcher
	for _, name := range cfg.Routing.Fallback.FetchChain() {
		switch name {
		case config.FallbackJina:
			remote = append(remote, fetch.NewJinaReader(&http.Client{Timeout: cfg.Server.RequestTimeout * 2}))
		}
	}
	return fetch.NewManager(remote, local, fetch.DefaultFetchCooldown, log)
}

// showQuota 打印各平台额度状态。
func showQuota(ctx context.Context, cfg *config.Config, configPath string) error {
	_, qm, st, err := build(cfg, configPath, logging.New(cfg.Server.LogLevel))
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Println("平台      周期            已用    剩余    上限   状态        真值")
	fmt.Println("--------  --------------  ------  ------  -----  ----------  --------")
	for _, name := range []string{config.ProviderExa, config.ProviderBrave, config.ProviderTavily} {
		pc, ok := cfg.Providers[name]
		if !ok {
			continue
		}
		if cfg.ResolveKey(name) == "" {
			fmt.Printf("%-8s  %-14s  %6s  %6s  %5s  %-10s  %s\n", name, "-", "-", "-", "-", "无密钥", "-")
			continue
		}
		status, err := qm.StatusOf(ctx, name, pc)
		if err != nil {
			return err
		}
		fmt.Printf("%-8s  %-14s  %6d  %6d  %5d  %-10s  %s\n",
			name, status.PeriodKey, status.LocalUsed, status.Remaining, status.Limit,
			string(status.State), status.Truth)
		if !status.ServerReset.IsZero() {
			fmt.Printf("          服务端重置时刻: %s\n", status.ServerReset.Format("2006-01-02 15:04:05"))
		}
		if status.LastError != "" {
			fmt.Printf("          最近错误: %s\n", truncate(status.LastError, 120))
		}
	}
	return nil
}

// showConfig 打印生效配置，密钥脱敏。
func showConfig(cfg *config.Config, configPath string) error {
	fmt.Printf("配置文件: %s\n", orDefault(configPath, "(未指定，使用默认配置)"))
	fmt.Printf("数据库:   %s\n", cfg.ResolveDBPath(configPath))
	fmt.Printf("超时:     %s\n", cfg.Server.RequestTimeout)
	fmt.Printf("日志级别: %s\n", cfg.Server.LogLevel)
	fmt.Printf("策略:     %s，最多尝试 %d 个平台\n", cfg.Routing.Strategy, cfg.Routing.MaxAttempts)
	fmt.Printf("免密钥兜底: %s（冷却 %s）\n",
		boolStr(cfg.Routing.Fallback.IsEnabled()), cfg.Routing.Fallback.CooldownDuration())
	if cfg.Routing.Fallback.IsEnabled() {
		fmt.Printf("  搜索链: %s\n", strings.Join(cfg.Routing.Fallback.SearchChain(), " → "))
		fmt.Printf("  抓取链: %s\n", strings.Join(cfg.Routing.Fallback.FetchChain(), " → "))
	}
	fmt.Println()
	fmt.Println("平台      启用  密钥                本地上限  周期模型")
	fmt.Println("--------  ----  ------------------  --------  ----------------")
	for _, name := range []string{config.ProviderExa, config.ProviderBrave, config.ProviderTavily} {
		pc, ok := cfg.Providers[name]
		if !ok {
			continue
		}
		fmt.Printf("%-8s  %-4s  %-18s  %8d  %s\n",
			name, boolStr(pc.Enabled), config.MaskKey(cfg.ResolveKey(name)),
			pc.MonthlyLimit, pc.PeriodModel)
	}
	return nil
}

// doctor 对各平台做一次最小请求自检。
func doctor(ctx context.Context, cfg *config.Config, configPath string) error {
	log := logging.New(cfg.Server.LogLevel)
	r, _, st, err := build(cfg, configPath, log)
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Println("平台连通性自检")
	fmt.Println("==============")
	for _, name := range r.Names() {
		t, _ := r.TargetByName(name)
		start := time.Now()
		resp, err := t.Provider.Search(ctx, provider.Query{Text: "websearch doctor check", NumResults: 1})
		cost := time.Since(start).Milliseconds()
		if err != nil {
			pe := provider.AsError(err)
			kind := "未知"
			if pe != nil {
				kind = string(pe.Kind)
			}
			fmt.Printf("  %-8s 失败  [%s] %v (%dms)\n", name, kind, err, cost)
			continue
		}
		fmt.Printf("  %-8s 正常  返回 %d 条，耗时 %dms\n", name, len(resp.Results), cost)
		if name == config.ProviderBrave {
			fmt.Printf("           速率限制: limit=%v remaining=%v reset=%v\n",
				resp.ServerLimit, resp.ServerRemaining, resp.ServerResetAt.Format("2006-01-02 15:04:05"))
		}
		if name == config.ProviderExa {
			fmt.Printf("           本次花费: $%.4f\n", resp.CostUSD)
		}
	}
	// 免密钥通道不扣额度，逐个检查以便提前发现限流或反爬问题。
	if cfg.Routing.Fallback.IsEnabled() {
		for _, name := range cfg.Routing.Fallback.SearchChain() {
			p := buildSearchFallback(name, cfg.Server.RequestTimeout)
			if p == nil {
				continue
			}
			checkFallback(ctx, p, os.Stdout)
		}
	} else {
		fmt.Println("  免密钥搜索通道已关闭")
	}
	return nil
}

// checkFallback 对免密钥通道做一次最小请求，并把结果写到 w。
// 该检查不写额度账本，也不会影响付费平台的调用统计。
func checkFallback(ctx context.Context, p provider.Provider, w io.Writer) {
	start := time.Now()
	resp, err := p.Search(ctx, provider.Query{Text: "websearch doctor check", NumResults: 1})
	cost := time.Since(start).Milliseconds()
	if err != nil {
		pe := provider.AsError(err)
		kind := "未知"
		if pe != nil {
			kind = string(pe.Kind)
		}
		fmt.Fprintf(w, "  %-8s 失败  [%s] %v (%dms)\n", p.Name(), kind, err, cost)
		return
	}
	fmt.Fprintf(w, "  %-8s 正常  返回 %d 条，耗时 %dms（免密钥，不扣额度）\n",
		p.Name(), len(resp.Results), cost)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
