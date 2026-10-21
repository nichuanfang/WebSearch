package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("文件不存在应返回错误")
	}
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("空路径应返回默认配置: %v", err)
	}
	if cfg2.Routing.MaxAttempts != 3 {
		t.Errorf("默认 max_attempts = %d, 期望 3", cfg2.Routing.MaxAttempts)
	}
	if cfg2.Providers[ProviderExa].MonthlyLimit != 1000 {
		t.Errorf("默认 exa 上限 = %d, 期望 1000", cfg2.Providers[ProviderExa].MonthlyLimit)
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  db_path: ./data/x.db
  request_timeout: 30s
  log_level: debug
providers:
  exa:
    enabled: true
    api_key: "file-key"
    monthly_limit: 500
    period_model: calendar_month
routing:
  max_attempts: 5
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.RequestTimeout != 30*time.Second {
		t.Errorf("超时 = %v, 期望 30s", cfg.Server.RequestTimeout)
	}
	if cfg.Providers[ProviderExa].MonthlyLimit != 500 {
		t.Errorf("exa 上限 = %d, 期望 500", cfg.Providers[ProviderExa].MonthlyLimit)
	}
	if cfg.Routing.MaxAttempts != 5 {
		t.Errorf("max_attempts = %d, 期望 5", cfg.Routing.MaxAttempts)
	}
	// 未出现的平台保留默认值。
	if cfg.Providers[ProviderTavily].MonthlyLimit != 1000 {
		t.Errorf("tavily 应保留默认上限, 实际 %d", cfg.Providers[ProviderTavily].MonthlyLimit)
	}
}

func TestEnvOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
providers:
  exa:
    api_key_env: MY_EXA_KEY
    api_key: "file-key"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	t.Setenv("MY_EXA_KEY", "env-key")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := cfg.ResolveKey(ProviderExa); got != "env-key" {
		t.Errorf("密钥 = %q, 期望环境变量 env-key 优先", got)
	}
}

func TestFallsBackToConfigFileKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
providers:
  exa:
    api_key_env: MISSING_ENV_VAR_X
    api_key: "file-key"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := cfg.ResolveKey(ProviderExa); got != "file-key" {
		t.Errorf("密钥 = %q, 期望回落到配置文件 file-key", got)
	}
}

func TestMissingKeyResolvesEmpty(t *testing.T) {
	cfg := Default()
	if got := cfg.ResolveKey("nonexistent"); got != "" {
		t.Errorf("未知平台密钥 = %q, 期望空", got)
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey(""); got != "(未配置)" {
		t.Errorf("空密钥脱敏 = %q", got)
	}
	if got := MaskKey("abcdefgh1234"); got != "***1234" {
		t.Errorf("脱敏结果 = %q, 期望 ***1234", got)
	}
}

func TestResolveDBPathRelativeToConfig(t *testing.T) {
	cfg := Default()
	cfg.Server.DBPath = "./data/websearch.db"
	got := cfg.ResolveDBPath("/opt/app/config.yaml")
	want := filepath.Join("/opt/app", "./data/websearch.db")
	if got != want {
		t.Errorf("数据库路径 = %q, 期望 %q", got, want)
	}
}

func TestFallbackDefaultsEnabled(t *testing.T) {
	cfg := Default()
	if !cfg.Routing.Fallback.IsEnabled() {
		t.Error("默认应启用免密钥兜底")
	}
	if cfg.Routing.Fallback.CooldownDuration() != DefaultFallbackCooldown {
		t.Errorf("默认冷却 = %v, 期望 %v", cfg.Routing.Fallback.CooldownDuration(), DefaultFallbackCooldown)
	}
}

func TestFallbackDefaultsWhenOmittedFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  max_attempts: 2
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.Routing.Fallback.IsEnabled() {
		t.Error("未写 fallback 时应默认启用")
	}
	if cfg.Routing.Fallback.CooldownDuration() != DefaultFallbackCooldown {
		t.Errorf("未写 cooldown 时应使用默认值, 实际 %v", cfg.Routing.Fallback.CooldownDuration())
	}
}

func TestFallbackExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  fallback:
    enabled: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Routing.Fallback.IsEnabled() {
		t.Error("显式 enabled: false 应关闭兜底")
	}
}

func TestFallbackCustomCooldown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  fallback:
    enabled: true
    cooldown: 3m
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := cfg.Routing.Fallback.CooldownDuration(); got != 3*time.Minute {
		t.Errorf("冷却 = %v, 期望 3m", got)
	}
}

func TestFallbackRejectsNonPositiveCooldown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  fallback:
    cooldown: 0s
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("显式 0 冷却应被拒绝")
	}
}

func TestFallbackDefaultChains(t *testing.T) {
	cfg := Default()
	search := cfg.Routing.Fallback.SearchChain()
	if len(search) != 2 || search[0] != FallbackParallel || search[1] != FallbackBing {
		t.Errorf("默认搜索链 = %v, 期望 [parallel bing]", search)
	}
	fetchChain := cfg.Routing.Fallback.FetchChain()
	if len(fetchChain) != 2 || fetchChain[0] != FallbackJina || fetchChain[1] != FallbackLocal {
		t.Errorf("默认抓取链 = %v, 期望 [jina local]", fetchChain)
	}
}

func TestFallbackChainsFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  fallback:
    search: [bing]
    fetch: [local]
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := cfg.Routing.Fallback.SearchChain(); len(got) != 1 || got[0] != FallbackBing {
		t.Errorf("搜索链 = %v, 期望 [bing]", got)
	}
	if got := cfg.Routing.Fallback.FetchChain(); len(got) != 1 || got[0] != FallbackLocal {
		t.Errorf("抓取链 = %v, 期望 [local]", got)
	}
}

func TestFallbackRejectsUnknownChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
routing:
  fallback:
    search: [unknown-engine]
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("未知搜索通道应被拒绝")
	}
}
