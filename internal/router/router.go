// Package router 实现负载均衡、熔断与优雅降级。
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/quota"
	"github.com/nichuanfang/WebSearch/internal/store"
)

// Target 是一个可被路由的平台。
type Target struct {
	Provider provider.Provider
	Config   config.ProviderConfig
	// HasKey 表示该平台是否配置了密钥。
	HasKey bool
}

// Router 负责选路、预扣、失败处理与降级。
type Router struct {
	targets []Target
	quota   *quota.Manager
	store   *store.Store
	log     *slog.Logger
	cursor  atomic.Uint64
	mu      sync.Mutex
}

// New 构造 Router。
func New(targets []Target, q *quota.Manager, s *store.Store, log *slog.Logger) *Router {
	return &Router{targets: targets, quota: q, store: s, log: log}
}

// Names 返回所有平台名称，按配置顺序稳定排列。
func (r *Router) Names() []string {
	out := make([]string, 0, len(r.targets))
	for _, t := range r.targets {
		out = append(out, t.Provider.Name())
	}
	return out
}

// TargetByName 按名称查找平台。
func (r *Router) TargetByName(name string) (Target, bool) {
	for _, t := range r.targets {
		if t.Provider.Name() == name {
			return t, true
		}
	}
	return Target{}, false
}

// Result 是一次成功搜索的结果。
type Result struct {
	Provider string
	Response *provider.Response
	// Attempts 记录实际尝试过的平台，顺序即尝试顺序。
	Attempts []Attempt
}

// Attempt 记录一次尝试的结果。
type Attempt struct {
	Provider string
	OK       bool
	Kind     string
	Message  string
}

// SearchResult 是搜索的对外结果。
type SearchResult struct {
	Result  *Result
	Skipped []string
}

// Search 在多个平台间选路并执行搜索，失败时按降级链自动切换。
// 平台顺序由 round_robin 游标决定，调用方无法指定。
func (r *Router) Search(ctx context.Context, q provider.Query, maxAttempts int) (*SearchResult, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	order := r.order()
	skipped := make([]string, 0, len(r.targets))
	attempts := make([]Attempt, 0, maxAttempts)

	for _, t := range order {
		if len(attempts) >= maxAttempts {
			break
		}
		if !t.HasKey {
			skipped = append(skipped, fmt.Sprintf("%s: 未配置密钥", t.Provider.Name()))
			continue
		}
		units := t.Provider.CostOf(q)
		gate, err := r.quota.CheckGate(ctx, t.Provider.Name(), t.Config, units)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %s", t.Provider.Name(), err.Error()))
			continue
		}
		if !gate.Allowed {
			skipped = append(skipped, fmt.Sprintf("%s: %s", t.Provider.Name(), gate.Reason))
			r.log.Debug("跳过平台", "provider", t.Provider.Name(), "reason", gate.Reason)
			continue
		}

		period, err := r.quota.CurrentPeriod(ctx, t.Provider.Name(), t.Config)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %s", t.Provider.Name(), err.Error()))
			continue
		}
		if err := r.quota.EnsureAnchor(ctx, t.Provider.Name(), t.Config, period); err != nil {
			r.log.Warn("记录锚点日失败", "provider", t.Provider.Name(), "error", err)
		}
		// 预扣，确保并发下不会超额。
		if err := r.store.Reserve(ctx, t.Provider.Name(), period.Key, period.Start, period.End, t.Config.MonthlyLimit, units); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: 预扣失败 %v", t.Provider.Name(), err))
			continue
		}

		start := time.Now()
		resp, err := t.Provider.Search(ctx, q)
		latency := time.Since(start).Milliseconds()

		if err != nil {
			perr := provider.AsError(err)
			kind := provider.KindTransient
			msg := err.Error()
			if perr != nil {
				kind = perr.Kind
				msg = perr.Message
			}
			attempts = append(attempts, Attempt{Provider: t.Provider.Name(), OK: false, Kind: string(kind), Message: msg})
			r.handleFailure(ctx, t, period, units, kind, msg, perr)
			r.log.Warn("平台调用失败", "provider", t.Provider.Name(), "kind", string(kind), "error", msg, "latency_ms", latency)
			continue
		}

		// 成功：预扣转为实际消耗。
		attempts = append(attempts, Attempt{Provider: t.Provider.Name(), OK: true})
		r.logSuccess(ctx, t, q, resp, latency, units)
		return &SearchResult{
			Result:  &Result{Provider: t.Provider.Name(), Response: resp, Attempts: attempts},
			Skipped: skipped,
		}, nil
	}

	return nil, &AllFailedError{Attempts: attempts, Skipped: skipped}
}

// AllFailedError 表示所有候选平台都不可用。
type AllFailedError struct {
	Attempts []Attempt
	Skipped  []string
}

func (e *AllFailedError) Error() string {
	var b []string
	for _, a := range e.Attempts {
		b = append(b, fmt.Sprintf("%s(%s): %s", a.Provider, a.Kind, a.Message))
	}
	for _, s := range e.Skipped {
		b = append(b, s)
	}
	if len(b) == 0 {
		return "所有搜索平台都不可用"
	}
	return "所有搜索平台都不可用 -> " + joinStrings(b, "; ")
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// order 计算本次尝试的平台顺序。
// round_robin：游标每次推进一格，保证三家额度均匀消耗。
func (r *Router) order() []Target {
	if len(r.targets) == 0 {
		return nil
	}
	start := int(r.cursor.Add(1)-1) % len(r.targets)
	out := make([]Target, 0, len(r.targets))
	for i := 0; i < len(r.targets); i++ {
		out = append(out, r.targets[(start+i)%len(r.targets)])
	}
	return out
}

// handleFailure 根据错误分类决定退还额度并迁移平台状态。
func (r *Router) handleFailure(ctx context.Context, t Target, period quota.Period, units float64, kind provider.ErrorKind, msg string, perr *provider.Error) {
	switch kind {
	case provider.KindQuotaExhausted:
		// 额度耗尽不扣额度，并标记平台为耗尽。
		if err := r.store.Refund(ctx, t.Provider.Name(), period.Key, units); err != nil {
			r.log.Error("退还额度失败", "provider", t.Provider.Name(), "error", err)
		}
		if err := r.store.SetState(ctx, t.Provider.Name(), period.Key, store.StateExhausted, time.Time{}, msg); err != nil {
			r.log.Error("更新平台状态失败", "provider", t.Provider.Name(), "error", err)
		}
	case provider.KindRateLimited:
		// 限速不扣额度，进入冷却。
		if err := r.store.Refund(ctx, t.Provider.Name(), period.Key, units); err != nil {
			r.log.Error("退还额度失败", "provider", t.Provider.Name(), "error", err)
		}
		cool := 60 * time.Second
		if perr != nil && perr.RetryAfter > 0 {
			cool = perr.RetryAfter
		}
		if err := r.store.SetState(ctx, t.Provider.Name(), period.Key, store.StateCooldown, time.Now().Add(cool), msg); err != nil {
			r.log.Error("更新平台状态失败", "provider", t.Provider.Name(), "error", err)
		}
	case provider.KindAuth:
		// 鉴权错误退还额度，但需人工干预，停止重试。
		if err := r.store.Refund(ctx, t.Provider.Name(), period.Key, units); err != nil {
			r.log.Error("退还额度失败", "provider", t.Provider.Name(), "error", err)
		}
		if err := r.store.SetState(ctx, t.Provider.Name(), period.Key, store.StateAuthError, time.Time{}, msg); err != nil {
			r.log.Error("更新平台状态失败", "provider", t.Provider.Name(), "error", err)
		}
	default:
		// 瞬时故障与参数错误退还额度。
		if err := r.store.Refund(ctx, t.Provider.Name(), period.Key, units); err != nil {
			r.log.Error("退还额度失败", "provider", t.Provider.Name(), "error", err)
		}
	}
}

// logSuccess 记录成功调用，并把服务端真值写回账本。
func (r *Router) logSuccess(ctx context.Context, t Target, q provider.Query, resp *provider.Response, latency int64, units float64) {
	hash := hashQuery(t.Provider.Name(), q.Text)
	entry := store.LogEntry{
		Provider:   t.Provider.Name(),
		QueryHash:  hash,
		NumResults: len(resp.Results),
		CostUnits:  units,
		LatencyMS:  int(latency),
		Status:     "ok",
	}
	if resp.CostUSD > 0 {
		entry.CostUSD = resp.CostUSD
		entry.HasCostUSD = true
	}
	if err := r.store.AppendLog(ctx, entry); err != nil {
		r.log.Error("写入调用日志失败", "provider", t.Provider.Name(), "error", err)
	}
	// Brave 会在响应头里带上服务端额度真值，Exa 可通过 costDollars 累加。
	if t.Provider.Name() == config.ProviderBrave && resp.ServerLimit >= 0 {
		serverUsed := 0.0
		if resp.ServerLimit > 0 {
			serverUsed = resp.ServerLimit - resp.ServerRemaining
		}
		if err := r.quota.ApplyServerTruth(ctx, t.Provider.Name(), t.Config, serverUsed, resp.ServerLimit, resp.ServerResetAt); err != nil {
			r.log.Error("写入服务端额度失败", "provider", t.Provider.Name(), "error", err)
		}
	}
	r.log.Info("搜索完成",
		"provider", t.Provider.Name(),
		"results", len(resp.Results),
		"cost_units", units,
		"cost_usd", resp.CostUSD,
		"latency_ms", latency)
}

// hashQuery 生成查询指纹，只保留哈希不落原文。
func hashQuery(provider, text string) string {
	h := sha256.New()
	h.Write([]byte(provider))
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// HashText 对查询文本求哈希，用于失败日志。
func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}
