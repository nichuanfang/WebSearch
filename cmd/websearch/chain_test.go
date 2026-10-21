package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/fetch"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildSearchFallbackByConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Fallback.TinyFishAPIKey = "tf-key"

	if p := buildSearchFallback(config.FallbackParallel, cfg, time.Second); p == nil || p.Name() != "parallel" {
		t.Errorf("parallel 通道构造失败")
	}
	p := buildSearchFallback(config.FallbackTinyFish, cfg, time.Second)
	if p == nil || p.Name() != "tinyfish" {
		t.Fatalf("tinyfish 通道构造失败")
	}
	if bing := buildSearchFallback(config.FallbackBing, cfg, time.Second); bing == nil || bing.Name() != "bing" {
		t.Errorf("bing 通道构造失败")
	}
	if unknown := buildSearchFallback("nope", cfg, time.Second); unknown != nil {
		t.Errorf("未知通道应返回 nil, 实际 %v", unknown.Name())
	}
}

func TestBuildFetcherChainOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Fallback.TinyFishAPIKey = "tf-key"
	m := buildFetcher(cfg, testLogger())
	if m == nil {
		t.Fatal("抓取管理器构造失败")
	}
	names := m.RemoteNames()
	want := []string{config.FallbackTinyFish, config.FallbackJina}
	if len(names) != len(want) {
		t.Fatalf("远程通道 = %v, 期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("远程通道 = %v, 期望 %v", names, want)
			break
		}
	}
}

func TestBuildFetcherSkipsTinyFishWithoutKey(t *testing.T) {
	cfg := config.Default()
	// 显式清空可能的密钥来源。
	cfg.Routing.Fallback.TinyFishAPIKey = ""
	t.Setenv(cfg.Routing.Fallback.TinyFishAPIKeyEnv, "")

	m := buildFetcher(cfg, testLogger())
	names := m.RemoteNames()
	if len(names) != 1 || names[0] != config.FallbackJina {
		t.Errorf("缺密钥时应只保留 jina, 实际 %v", names)
	}
}

func TestBuildFetcherDisabledUsesLocalOnly(t *testing.T) {
	cfg := config.Default()
	off := false
	cfg.Routing.Fallback.Enabled = &off
	m := buildFetcher(cfg, testLogger())
	if got := m.RemoteNames(); len(got) != 0 {
		t.Errorf("关闭兜底后不应有远程通道, 实际 %v", got)
	}
	_ = fetch.DefaultFetchCooldown
}
