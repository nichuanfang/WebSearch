package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/quota"
	"github.com/nichuanfang/WebSearch/internal/store"
)

// fakeProvider 是可控的测试替身。
type fakeProvider struct {
	name    string
	calls   int
	err     error
	cost    float64
	results []provider.Result
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) CostOf(provider.Query) float64 {
	if f.cost > 0 {
		return f.cost
	}
	return 1
}

func (f *fakeProvider) Search(ctx context.Context, q provider.Query) (*provider.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	res := f.results
	if res == nil {
		res = []provider.Result{{Title: "标题", URL: "https://example.com", Snippet: "摘要"}}
	}
	return &provider.Response{Provider: f.name, Results: res, CostUnits: f.CostOf(q)}, nil
}

func newTestRouter(t *testing.T, targets ...Target) (*Router, *store.Store, *quota.Manager) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	qm := quota.NewManager(s, time.Now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(targets, qm, s, log), s, qm
}

func target(name string, p provider.Provider, limit int) Target {
	return Target{
		Provider: p,
		Config: config.ProviderConfig{
			Enabled: true, MonthlyLimit: limit, PeriodModel: config.PeriodCalendarMonth,
		},
		HasKey: true,
	}
}

func TestRoundRobinRotatesProviders(t *testing.T) {
	exa := &fakeProvider{name: "exa"}
	brave := &fakeProvider{name: "brave"}
	tavily := &fakeProvider{name: "tavily"}
	r, _, _ := newTestRouter(t,
		target("exa", exa, 100),
		target("brave", brave, 100),
		target("tavily", tavily, 100),
	)
	ctx := context.Background()
	hit := map[string]int{}
	for i := 0; i < 3; i++ {
		res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
		if err != nil {
			t.Fatalf("第 %d 次搜索失败: %v", i+1, err)
		}
		hit[res.Result.Provider]++
	}
	if hit["exa"] != 1 || hit["brave"] != 1 || hit["tavily"] != 1 {
		t.Errorf("三次搜索应各命中一个平台, 实际 %v", hit)
	}
}

func TestFallbackOnQuotaExhausted(t *testing.T) {
	exa := &fakeProvider{name: "exa", err: provider.NewError("exa", provider.KindQuotaExhausted, 402, "余额耗尽")}
	brave := &fakeProvider{name: "brave"}
	r, _, _ := newTestRouter(t, target("exa", exa, 100), target("brave", brave, 100))
	ctx := context.Background()

	res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("应降级到 brave 成功, 实际错误: %v", err)
	}
	if res.Result.Provider != "brave" {
		t.Errorf("实际命中平台 = %q, 期望 brave", res.Result.Provider)
	}
	// exa 额度耗尽不应扣额度。
	q, _ := r.store.GetQuota(ctx, "exa", currentKey(t, r, "exa"))
	if q != nil && q.LocalUsed != 0 {
		t.Errorf("额度耗尽不应扣额度, 实际已用 %d", q.LocalUsed)
	}
	// 状态应被标记为 exhausted。
	if q == nil || q.State != store.StateExhausted {
		t.Errorf("exa 状态应为 exhausted, 实际 %+v", q)
	}
}

func TestFallbackOnRateLimit(t *testing.T) {
	brave := &fakeProvider{name: "brave", err: provider.NewError("brave", provider.KindRateLimited, 429, "限速")}
	exa := &fakeProvider{name: "exa"}
	r, _, _ := newTestRouter(t, target("brave", brave, 100), target("exa", exa, 100))
	ctx := context.Background()

	res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("应降级成功: %v", err)
	}
	if res.Result.Provider != "exa" {
		t.Errorf("实际命中平台 = %q, 期望 exa", res.Result.Provider)
	}
	q, _ := r.store.GetQuota(ctx, "brave", currentKey(t, r, "brave"))
	if q == nil || q.State != store.StateCooldown {
		t.Errorf("brave 状态应为 cooldown, 实际 %+v", q)
	}
	if q.LocalUsed != 0 {
		t.Errorf("限速不应扣额度, 实际已用 %d", q.LocalUsed)
	}
}

func TestAuthErrorStopsRetry(t *testing.T) {
	exa := &fakeProvider{name: "exa", err: provider.NewError("exa", provider.KindAuth, 401, "密钥无效")}
	brave := &fakeProvider{name: "brave"}
	r, _, _ := newTestRouter(t, target("exa", exa, 100), target("brave", brave, 100))
	ctx := context.Background()

	if _, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3); err != nil {
		t.Fatalf("应降级成功: %v", err)
	}
	q, _ := r.store.GetQuota(ctx, "exa", currentKey(t, r, "exa"))
	if q == nil || q.State != store.StateAuthError {
		t.Errorf("exa 状态应为 auth_error, 实际 %+v", q)
	}
	if q.LocalUsed != 0 {
		t.Errorf("鉴权失败不应扣额度, 实际已用 %d", q.LocalUsed)
	}
}

func TestTransientErrorRefunds(t *testing.T) {
	exa := &fakeProvider{name: "exa", err: provider.NewError("exa", provider.KindTransient, 500, "服务端错误")}
	brave := &fakeProvider{name: "brave"}
	r, _, _ := newTestRouter(t, target("exa", exa, 100), target("brave", brave, 100))
	ctx := context.Background()

	if _, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3); err != nil {
		t.Fatalf("应降级成功: %v", err)
	}
	q, _ := r.store.GetQuota(ctx, "exa", currentKey(t, r, "exa"))
	if q == nil || q.LocalUsed != 0 {
		t.Errorf("瞬时故障应退还额度, 实际 %+v", q)
	}
}

func TestSkipsProviderWithoutKey(t *testing.T) {
	exa := &fakeProvider{name: "exa"}
	brave := &fakeProvider{name: "brave"}
	tgt := target("brave", brave, 100)
	r, _, _ := newTestRouter(t, Target{
		Provider: exa, HasKey: false,
		Config: config.ProviderConfig{MonthlyLimit: 100, PeriodModel: config.PeriodCalendarMonth},
	}, tgt)
	ctx := context.Background()

	res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if res.Result.Provider != "brave" {
		t.Errorf("无密钥平台应被跳过, 实际命中 %q", res.Result.Provider)
	}
	if exa.calls != 0 {
		t.Error("无密钥平台不应被调用")
	}
}

func TestRespectsMaxAttempts(t *testing.T) {
	all := func() *fakeProvider {
		return &fakeProvider{name: "p", err: provider.NewError("p", provider.KindTransient, 500, "失败")}
	}
	a, b, c := all(), all(), all()
	a.name, b.name, c.name = "exa", "brave", "tavily"
	r, _, _ := newTestRouter(t,
		target("exa", a, 100), target("brave", b, 100), target("tavily", c, 100))
	ctx := context.Background()

	_, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 2)
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	var allFailed *AllFailedError
	if !errors.As(err, &allFailed) {
		t.Fatalf("错误类型应为 AllFailedError, 实际 %T", err)
	}
	if len(allFailed.Attempts) != 2 {
		t.Errorf("尝试次数 = %d, 期望受 max_attempts=2 限制", len(allFailed.Attempts))
	}
}

func TestAllProvidersExhaustedReturnsError(t *testing.T) {
	exa := &fakeProvider{name: "exa", err: provider.NewError("exa", provider.KindQuotaExhausted, 402, "耗尽")}
	r, _, _ := newTestRouter(t, target("exa", exa, 100))
	ctx := context.Background()

	_, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err == nil {
		t.Fatal("应返回错误")
	}
	var allFailed *AllFailedError
	if !errors.As(err, &allFailed) {
		t.Fatalf("错误类型应为 AllFailedError, 实际 %T", err)
	}
}

func TestRoundRobinIgnoresAnyProviderPreference(t *testing.T) {
	// 调用方无法指定平台：轮询顺序完全由游标决定。
	exa := &fakeProvider{name: "exa"}
	brave := &fakeProvider{name: "brave"}
	r, _, _ := newTestRouter(t, target("exa", exa, 100), target("brave", brave, 100))
	ctx := context.Background()

	first, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	second, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if first.Result.Provider == second.Result.Provider {
		t.Errorf("连续两次应命中不同平台, 均为 %q", first.Result.Provider)
	}
}

func TestFailedPlatformFallsBackToNext(t *testing.T) {
	exa := &fakeProvider{name: "exa"}
	brave := &fakeProvider{name: "brave", err: provider.NewError("brave", provider.KindTransient, 500, "失败")}
	r, _, _ := newTestRouter(t, target("exa", exa, 100), target("brave", brave, 100))
	ctx := context.Background()

	res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
	if err != nil {
		t.Fatalf("应降级成功: %v", err)
	}
	if res.Result.Provider != "exa" {
		t.Errorf("应降级到 exa, 实际命中 %q", res.Result.Provider)
	}
}

func TestLocalLimitBlocksProvider(t *testing.T) {
	exa := &fakeProvider{name: "exa"}
	brave := &fakeProvider{name: "brave"}
	r, _, _ := newTestRouter(t, target("exa", exa, 2), target("brave", brave, 100))
	ctx := context.Background()

	// exa 上限为 2。连续调用直到 exa 被闸门拦下，
	// 此时必须降级到 brave 而不是报错。
	var hitExa, hitBrave int
	for i := 0; i < 6; i++ {
		res, err := r.Search(ctx, provider.Query{Text: "q", NumResults: 1}, 3)
		if err != nil {
			t.Fatalf("第 %d 次搜索失败: %v", i+1, err)
		}
		switch res.Result.Provider {
		case "exa":
			hitExa++
		case "brave":
			hitBrave++
		}
	}
	if hitExa > 2 {
		t.Errorf("exa 命中 %d 次, 超过上限 2", hitExa)
	}
	if hitBrave == 0 {
		t.Error("exa 超限后应降级到 brave")
	}
}

func currentKey(t *testing.T, r *Router, name string) string {
	t.Helper()
	tgt, ok := r.TargetByName(name)
	if !ok {
		t.Fatalf("找不到平台 %s", name)
	}
	period := r.quota.PeriodFor(tgt.Config.PeriodModel, time.Time{}, time.Now())
	return period.Key
}
