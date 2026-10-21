// Package config 负责加载 YAML 配置并应用环境变量覆盖。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// 平台名称常量。
const (
	ProviderExa    = "exa"
	ProviderBrave  = "brave"
	ProviderTavily = "tavily"
)

// PeriodModel 表示额度周期模型。
type PeriodModel string

const (
	// PeriodCalendarMonth 自然月，每月 1 号归零（Exa）。
	PeriodCalendarMonth PeriodModel = "calendar_month"
	// PeriodRolling31D 滚动 31 天（Brave，服务端 policy 实测 w=2678400）。
	PeriodRolling31D PeriodModel = "rolling_31d"
	// PeriodRolling1M 滚动 1 个月（Tavily 账单周期近似）。
	PeriodRolling1M PeriodModel = "rolling_1m"
)

// Config 是配置文件的根结构。
type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Routing   RoutingConfig             `yaml:"routing"`
}

// ServerConfig 是服务端通用配置。
type ServerConfig struct {
	DBPath         string        `yaml:"db_path"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	LogLevel       string        `yaml:"log_level"`
}

// ProviderConfig 是单个平台的配置。
type ProviderConfig struct {
	Enabled         bool        `yaml:"enabled"`
	APIKeyEnv       string      `yaml:"api_key_env"`
	APIKey          string      `yaml:"api_key"`
	MonthlyLimit    int         `yaml:"monthly_limit"`
	MonthlyLimitUSD float64     `yaml:"monthly_limit_usd"`
	PeriodModel     PeriodModel `yaml:"period_model"`
}

// RoutingConfig 是路由与降级策略配置。
type RoutingConfig struct {
	Strategy    string         `yaml:"strategy"`
	MaxAttempts int            `yaml:"max_attempts"`
	Fallback    FallbackConfig `yaml:"fallback"`
}

// FallbackConfig 是免密钥补充通道的配置。
type FallbackConfig struct {
	// Enabled 为零值时按默认开启处理：只有显式写 false 才关闭。
	Enabled *bool `yaml:"enabled"`
	// Cooldown 是兜底遇到限速或反爬后的冷却时长。
	Cooldown *time.Duration `yaml:"cooldown"`
	// Search 是免密钥搜索通道的优先级顺序。
	Search []string `yaml:"search"`
	// Fetch 是免密钥网页抓取通道的优先级顺序。
	Fetch []string `yaml:"fetch"`
}

// DefaultFallbackCooldown 是兜底失败的默认冷却时长。
const DefaultFallbackCooldown = 10 * time.Minute

// 免密钥通道名称。
const (
	// FallbackParallel 是 Parallel 免密钥搜索/抓取。
	FallbackParallel = "parallel"
	// FallbackBing 是 Bing RSS 免密钥搜索。
	FallbackBing = "bing"
	// FallbackJina 是 Jina Reader 免密钥抓取。
	FallbackJina = "jina"
	// FallbackLocal 是纯本地网页抓取。
	FallbackLocal = "local"
)

// IsEnabled 返回兜底是否启用；未配置时默认启用。
func (f FallbackConfig) IsEnabled() bool {
	return f.Enabled == nil || *f.Enabled
}

// CooldownDuration 返回兜底冷却时长；未配置时使用默认值。
func (f FallbackConfig) CooldownDuration() time.Duration {
	if f.Cooldown == nil {
		return DefaultFallbackCooldown
	}
	return *f.Cooldown
}

// SearchChain 返回免密钥搜索通道顺序；未配置时默认 Parallel → Bing。
func (f FallbackConfig) SearchChain() []string {
	if len(f.Search) == 0 {
		return []string{FallbackParallel, FallbackBing}
	}
	return f.Search
}

// FetchChain 返回免密钥抓取通道顺序；未配置时默认 Jina → 本地。
func (f FallbackConfig) FetchChain() []string {
	if len(f.Fetch) == 0 {
		return []string{FallbackJina, FallbackLocal}
	}
	return f.Fetch
}

// Default 返回带默认值的配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			DBPath:         "./data/websearch.db",
			RequestTimeout: 20 * time.Second,
			LogLevel:       "info",
		},
		Providers: map[string]ProviderConfig{
			ProviderExa: {
				Enabled:         true,
				APIKeyEnv:       "EXA_API_KEY",
				MonthlyLimit:    1000,
				MonthlyLimitUSD: 10.0,
				PeriodModel:     PeriodCalendarMonth,
			},
			ProviderBrave: {
				Enabled:      true,
				APIKeyEnv:    "BRAVE_API_KEY",
				MonthlyLimit: 1000,
				PeriodModel:  PeriodRolling31D,
			},
			ProviderTavily: {
				Enabled:      true,
				APIKeyEnv:    "TAVILY_API_KEY",
				MonthlyLimit: 1000,
				PeriodModel:  PeriodRolling1M,
			},
		},
		Routing: RoutingConfig{
			Strategy:    "round_robin",
			MaxAttempts: 3,
			Fallback: FallbackConfig{
				Enabled:  boolPtr(true),
				Cooldown: durationPtr(DefaultFallbackCooldown),
				Search:   []string{FallbackParallel, FallbackBing},
				Fetch:    []string{FallbackJina, FallbackLocal},
			},
		},
	}
}

func durationPtr(d time.Duration) *time.Duration {
	return &d
}

// Load 从 path 读取配置；path 为空时返回默认配置。
// 未在 YAML 中出现的字段保留默认值。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("校验配置文件 %s: %w", path, err)
	}
	return cfg, nil
}

// normalize 修正零值字段。
func (c *Config) normalize() {
	if c.Server.DBPath == "" {
		c.Server.DBPath = "./data/websearch.db"
	}
	if c.Server.RequestTimeout <= 0 {
		c.Server.RequestTimeout = 20 * time.Second
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Routing.MaxAttempts <= 0 {
		c.Routing.MaxAttempts = 3
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "round_robin"
	}
	if c.Routing.Fallback.Enabled == nil {
		c.Routing.Fallback.Enabled = boolPtr(true)
	}
	if c.Routing.Fallback.Cooldown == nil {
		c.Routing.Fallback.Cooldown = durationPtr(DefaultFallbackCooldown)
	}
	if len(c.Routing.Fallback.Search) == 0 {
		c.Routing.Fallback.Search = []string{FallbackParallel, FallbackBing}
	}
	if len(c.Routing.Fallback.Fetch) == 0 {
		c.Routing.Fallback.Fetch = []string{FallbackJina, FallbackLocal}
	}
	for name, p := range c.Providers {
		if p.MonthlyLimit <= 0 {
			p.MonthlyLimit = 1000
		}
		if p.PeriodModel == "" {
			p.PeriodModel = defaultPeriodModel(name)
		}
		c.Providers[name] = p
	}
}

// validate 校验显式配置的值，拒绝会让兜底行为含糊的输入。
func (c *Config) validate() error {
	if c.Routing.Fallback.Cooldown != nil && *c.Routing.Fallback.Cooldown <= 0 {
		return fmt.Errorf("routing.fallback.cooldown 必须大于 0")
	}
	for _, name := range c.Routing.Fallback.Search {
		if name != FallbackParallel && name != FallbackBing {
			return fmt.Errorf("routing.fallback.search 含未知通道 %q（可选 parallel、bing）", name)
		}
	}
	for _, name := range c.Routing.Fallback.Fetch {
		if name != FallbackJina && name != FallbackLocal {
			return fmt.Errorf("routing.fallback.fetch 含未知通道 %q（可选 jina、local）", name)
		}
	}
	return nil
}

func boolPtr(v bool) *bool {
	return &v
}

func defaultPeriodModel(name string) PeriodModel {
	switch name {
	case ProviderExa:
		return PeriodCalendarMonth
	case ProviderBrave:
		return PeriodRolling31D
	case ProviderTavily:
		return PeriodRolling1M
	}
	return PeriodCalendarMonth
}

// ResolveKey 按"环境变量优先、配置文件次之"的顺序解析某个平台的密钥。
// 两者都为空时返回空字符串，表示该平台未配置。
func (c *Config) ResolveKey(name string) string {
	p, ok := c.Providers[name]
	if !ok {
		return ""
	}
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v
		}
	}
	return p.APIKey
}

// MaskKey 将密钥脱敏，仅保留末 4 位。
func MaskKey(key string) string {
	if key == "" {
		return "(未配置)"
	}
	if len(key) <= 4 {
		return "***"
	}
	return "***" + key[len(key)-4:]
}

// ResolveDBPath 返回相对于配置目录解析后的数据库路径。
func (c *Config) ResolveDBPath(configPath string) string {
	p := c.Server.DBPath
	if filepath.IsAbs(p) || configPath == "" {
		return p
	}
	return filepath.Join(filepath.Dir(configPath), p)
}
