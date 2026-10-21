package search

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/quota"
	"github.com/nichuanfang/WebSearch/internal/router"
	"github.com/nichuanfang/WebSearch/internal/store"
)

// fakePlatform 模拟一个上游平台的可控行为。
type fakePlatform struct {
	name       string
	status     int
	body       string
	calls      int
	lastBody   map[string]any
	rateHeader string
	server     *httptest.Server
}

func (f *fakePlatform) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		raw, _ := io.ReadAll(r.Body)
		f.lastBody = map[string]any{}
		json.Unmarshal(raw, &f.lastBody)
		if f.rateHeader != "" {
			w.Header().Set("x-ratelimit-limit", f.rateHeader)
			w.Header().Set("x-ratelimit-remaining", f.rateHeader)
		}
		if f.status != 0 && f.status != 200 {
			w.WriteHeader(f.status)
			w.Write([]byte(f.body))
			return
		}
		w.Write([]byte(f.body))
	}
}

func newService(t *testing.T, platforms map[string]*fakePlatform, order []string, limits map[string]int) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	qm := quota.NewManager(s, time.Now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var targets []router.Target
	for _, name := range order {
		p := platforms[name]
		var prov provider.Provider
		switch name {
		case config.ProviderExa:
			e := provider.NewExa("k", p.server.Client())
			e.BaseURL = p.server.URL
			prov = e
		case config.ProviderBrave:
			b := provider.NewBrave("k", p.server.Client())
			b.BaseURL = p.server.URL
			prov = b
		case config.ProviderTavily:
			tv := provider.NewTavily("k", p.server.Client())
			tv.BaseURL = p.server.URL
			prov = tv
		}
		limit := 1000
		if l, ok := limits[name]; ok {
			limit = l
		}
		targets = append(targets, router.Target{
			Provider: prov,
			Config: config.ProviderConfig{
				Enabled: true, MonthlyLimit: limit, PeriodModel: config.PeriodCalendarMonth,
			},
			HasKey: true,
		})
	}
	r := router.New(targets, qm, s, log)
	return New(r, log, 3), s
}

func newPlatform(name string, status int, body string) *fakePlatform {
	p := &fakePlatform{name: name, status: status, body: body}
	p.server = httptest.NewServer(p.handler())
	return p
}

const exaOK = `{"results":[{"title":"Exa标题","url":"https://exa.com","highlights":["exa's"]}],"costDollars":{"total":0.004}}`
const braveOK = `{"web":{"results":[{"title":"Brave标题","url":"https://brave.com","description":"brave的"}]}}`
const tavilyOK = `{"results":[{"title":"Tavily标题","url":"https://tavily.com","content":"tavily的","score":0.9}]}`

func TestIntegrationFallbackAcrossAllThree(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"NO_MORE_CREDITS","tag":"NO_MORE_CREDITS"}`)
	brave := newPlatform("brave", 429, `{"error":"slow down"}`)
	tavily := newPlatform("tavily", 200, tavilyOK)
	defer exa.server.Close()
	defer brave.server.Close()
	defer tavily.server.Close()

	svc, _ := newService(t,
		map[string]*fakePlatform{"exa": exa, "brave": brave, "tavily": tavily},
		[]string{"exa", "brave", "tavily"}, nil)

	resp, err := svc.Execute(context.Background(), Request{Query: "测试", NumResults: 3})
	if err != nil {
		t.Fatalf("应降级到 tavily 成功, 实际: %v", err)
	}
	if len(resp.Results) == 0 || resp.Results[0].Title != "Tavily标题" {
		t.Errorf("应降级到 tavily, 实际结果 %+v", resp.Results)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "Tavily标题" {
		t.Errorf("结果异常: %+v", resp.Results)
	}
	// 三家都应被调用过。
	if exa.calls == 0 || brave.calls == 0 || tavily.calls == 0 {
		t.Errorf("降级链应依次尝试三家, 调用次数 exa=%d brave=%d tavily=%d", exa.calls, brave.calls, tavily.calls)
	}
}

func TestIntegrationAllFailReturnsError(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	brave := newPlatform("brave", 402, `{"error":"no credits"}`)
	tavily := newPlatform("tavily", 432, `{"detail":{"error":"plan limit"}}`)
	defer exa.server.Close()
	defer brave.server.Close()
	defer tavily.server.Close()

	svc, _ := newService(t,
		map[string]*fakePlatform{"exa": exa, "brave": brave, "tavily": tavily},
		[]string{"exa", "brave", "tavily"}, nil)

	_, err := svc.Execute(context.Background(), Request{Query: "测试"})
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "不可用") {
		t.Errorf("错误文案 = %q, 应说明所有平台不可用", err.Error())
	}
}

func TestIntegrationTavilyCostsOneUnit(t *testing.T) {
	tavily := newPlatform("tavily", 200, tavilyOK)
	defer tavily.server.Close()

	svc, s := newService(t,
		map[string]*fakePlatform{"tavily": tavily},
		[]string{"tavily"}, nil)

	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	q, err := s.GetQuota(context.Background(), "tavily", "2026-10")
	if err != nil {
		t.Fatalf("读取额度失败: %v", err)
	}
	// 周期 key 取决于当前月份，改为遍历查找。
	if q == nil {
		q = findQuota(t, s, "tavily")
	}
	if q == nil || q.LocalUsed != 1 {
		t.Errorf("应扣 1 credit, 实际 %+v", q)
	}
}

func findQuota(t *testing.T, s *store.Store, provider string) *store.Quota {
	t.Helper()
	// 尝试相邻几个周期 key。
	for _, key := range []string{"2026-10", "2026-09", "2026-11"} {
		q, err := s.GetQuota(context.Background(), provider, key)
		if err == nil && q != nil {
			return q
		}
	}
	return nil
}

func TestIntegrationExaRequestExcludesSummary(t *testing.T) {
	exa := newPlatform("exa", 200, exaOK)
	defer exa.server.Close()

	svc, _ := newService(t,
		map[string]*fakePlatform{"exa": exa},
		[]string{"exa"}, nil)

	if _, err := svc.Execute(context.Background(), Request{Query: "测试", NumResults: 10}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	contents, ok := exa.lastBody["contents"].(map[string]any)
	if !ok {
		t.Fatal("请求体缺少 contents")
	}
	if _, bad := contents["summary"]; bad {
		t.Error("请求体不得包含 contents.summary")
	}
}

func TestIntegrationBraveUnlimitedLimitNotBlocked(t *testing.T) {
	// Brave 实测月度上限为 0（不限量），本地闸门不应误判为耗尽。
	brave := newPlatform("brave", 200, braveOK)
	brave.rateHeader = "50, 0"
	defer brave.server.Close()

	svc, _ := newService(t,
		map[string]*fakePlatform{"brave": brave},
		[]string{"brave"}, nil)

	for i := 0; i < 3; i++ {
		if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
			t.Fatalf("第 %d 次搜索失败: %v", i+1, err)
		}
	}
}

func TestIntegrationLocalLimitTriggersFallback(t *testing.T) {
	exa := newPlatform("exa", 200, exaOK)
	tavily := newPlatform("tavily", 200, tavilyOK)
	defer exa.server.Close()
	defer tavily.server.Close()

	svc, _ := newService(t,
		map[string]*fakePlatform{"exa": exa, "tavily": tavily},
		[]string{"exa", "tavily"},
		map[string]int{"exa": 2, "tavily": 100})

	// exa 上限 2，轮询会反复回到 exa。调用足够多次后，
	// exa 必须被本地闸门拦下并降级到 tavily，而不是报错。
	for i := 0; i < 6; i++ {
		if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
			t.Fatalf("第 %d 次搜索失败: %v", i+1, err)
		}
	}
	// exa 上限 2，被闸门拦下后必然调用 tavily。
	if exa.calls > 2 {
		t.Errorf("exa 调用 %d 次, 超过上限 2", exa.calls)
	}
	if tavily.calls == 0 {
		t.Error("exa 超限后应降级到 tavily")
	}
}

func TestIntegrationEmptyQueryRejected(t *testing.T) {
	exa := newPlatform("exa", 200, exaOK)
	defer exa.server.Close()
	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)

	if _, err := svc.Execute(context.Background(), Request{Query: "   "}); err == nil {
		t.Error("空查询应被拒绝")
	}
}

// fakeFallback 是免密钥兜底通道的可控测试替身。
type fakeFallback struct {
	name    string
	results []provider.Result
	err     error
	calls   int
	last    provider.Query
}

func (f *fakeFallback) Name() string {
	if f.name != "" {
		return f.name
	}
	return provider.BingName
}
func (f *fakeFallback) CostOf(provider.Query) float64 { return 0 }
func (f *fakeFallback) Search(_ context.Context, q provider.Query) (*provider.Response, error) {
	f.calls++
	f.last = q
	if f.err != nil {
		return nil, f.err
	}
	return &provider.Response{Provider: f.Name(), Results: f.results}, nil
}

func TestFallbackTriggersWhenAllPaidProvidersFail(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	fb := &fakeFallback{results: []provider.Result{{Title: "兜底标题", URL: "https://bing.example/1"}}}
	svc.WithFallback(fb, 10*time.Minute)

	resp, err := svc.Execute(context.Background(), Request{Query: "测试", NumResults: 3})
	if err != nil {
		t.Fatalf("应兜底成功, 实际: %v", err)
	}
	if fb.calls != 1 {
		t.Errorf("兜底调用次数 = %d, 期望 1", fb.calls)
	}
	if fb.last.NumResults != 3 || fb.last.Text != "测试" {
		t.Errorf("兜底入参异常: %+v", fb.last)
	}
	if resp.Count != 1 || resp.Results[0].Title != "兜底标题" {
		t.Errorf("兜底结果异常: %+v", resp)
	}
}

func TestFallbackNotTriggeredWhenPaidProviderSucceedsEmpty(t *testing.T) {
	exa := newPlatform("exa", 200, `{"results":[]}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	fb := &fakeFallback{results: []provider.Result{{Title: "兜底标题", URL: "https://bing.example/1"}}}
	svc.WithFallback(fb, 10*time.Minute)

	resp, err := svc.Execute(context.Background(), Request{Query: "测试"})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if fb.calls != 0 {
		t.Errorf("付费平台成功时不应调用兜底, 实际 %d 次", fb.calls)
	}
	if resp.Count != 0 {
		t.Errorf("应返回付费平台的空结果, 实际 %+v", resp)
	}
}

func TestFallbackFailureReturnsUnifiedErrorWithReasons(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	fb := &fakeFallback{err: provider.NewError(provider.BingName, provider.KindTransient, 500, "rss down")}
	svc.WithFallback(fb, 10*time.Minute)

	_, err := svc.Execute(context.Background(), Request{Query: "测试"})
	if err == nil {
		t.Fatal("兜底也失败时应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "所有搜索平台都不可用") {
		t.Errorf("错误文案 = %q, 应说明所有平台不可用", msg)
	}
	// 回归：前缀只能出现一次，避免 "所有搜索平台都不可用 -> 所有搜索平台都不可用"。
	if strings.Count(msg, "所有搜索平台都不可用") != 1 {
		t.Errorf("错误文案 = %q, 前缀重复出现", msg)
	}
	if !strings.Contains(msg, "exa") {
		t.Errorf("错误文案 = %q, 应保留付费平台原因", msg)
	}
	if !strings.Contains(msg, "bing") {
		t.Errorf("错误文案 = %q, 应保留兜底原因", msg)
	}
}

func TestFallbackCooldownSkipsRepeatedRequests(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	svc.now = func() time.Time { return now }
	fb := &fakeFallback{err: provider.NewError(provider.BingName, provider.KindRateLimited, 429, "blocked")}
	svc.WithFallback(fb, 10*time.Minute)

	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err == nil {
		t.Fatal("首次失败应返回错误")
	}
	if fb.calls != 1 {
		t.Fatalf("首次调用后兜底次数 = %d, 期望 1", fb.calls)
	}

	// 冷却期内不应再次请求 Bing。
	now = now.Add(5 * time.Minute)
	_, err := svc.Execute(context.Background(), Request{Query: "测试"})
	if err == nil {
		t.Fatal("冷却期内应返回错误")
	}
	if !strings.Contains(err.Error(), "冷却中") {
		t.Errorf("错误文案 = %q, 应说明冷却中", err.Error())
	}
	if fb.calls != 1 {
		t.Errorf("冷却期内不应重复请求, 实际 %d 次", fb.calls)
	}

	// 冷却结束后恢复尝试。
	now = now.Add(6 * time.Minute)
	fb.err = nil
	fb.results = []provider.Result{{Title: "恢复", URL: "https://bing.example/2"}}
	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
		t.Fatalf("冷却结束后应恢复兜底: %v", err)
	}
	if fb.calls != 2 {
		t.Errorf("冷却结束后应再次请求, 实际 %d 次", fb.calls)
	}
}

func TestFallbackTransientErrorDoesNotEnterCooldown(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	fb := &fakeFallback{err: provider.NewError(provider.BingName, provider.KindTransient, 500, "rss down")}
	svc.WithFallback(fb, 10*time.Minute)

	for i := 0; i < 2; i++ {
		if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err == nil {
			t.Fatal("应返回错误")
		}
	}
	if fb.calls != 2 {
		t.Errorf("瞬时故障不应进入冷却, 兜底次数 = %d, 期望 2", fb.calls)
	}
}

func TestFallbackDisabledDoesNotCallFallback(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()

	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)
	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err == nil {
		t.Fatal("未启用兜底时应返回错误")
	}
}

func TestFallbackChainPrefersFirstAndFallsThrough(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()
	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)

	parallel := &fakeFallback{name: provider.ParallelName, err: provider.NewError(provider.ParallelName, provider.KindRateLimited, 429, "Too Many Requests")}
	bing := &fakeFallback{name: provider.BingName, results: []provider.Result{{Title: "Bing 结果", URL: "https://bing.example/1"}}}
	svc.WithFallbackOptions(parallel, 10*time.Minute, false)
	svc.WithFallbackOptions(bing, 10*time.Minute, true)

	resp, err := svc.Execute(context.Background(), Request{Query: "Go 泛型 使用教程"})
	if err != nil {
		t.Fatalf("Parallel 失败后应降级到 Bing: %v", err)
	}
	if parallel.calls != 1 || bing.calls != 1 {
		t.Errorf("调用次数 parallel=%d bing=%d, 期望各 1 次", parallel.calls, bing.calls)
	}
	if resp.Results[0].Title != "Bing 结果" {
		t.Errorf("应返回 Bing 结果, 实际 %+v", resp.Results)
	}
}

func TestFallbackChainSkipsCoolingProvider(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()
	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	svc.now = func() time.Time { return now }
	parallel := &fakeFallback{name: provider.ParallelName, err: provider.NewError(provider.ParallelName, provider.KindRateLimited, 429, "429")}
	bing := &fakeFallback{name: provider.BingName, results: []provider.Result{{Title: "Bing", URL: "https://bing.example/1"}}}
	svc.WithFallbackOptions(parallel, 10*time.Minute, false)
	svc.WithFallbackOptions(bing, 10*time.Minute, true)

	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
		t.Fatalf("首次应降级到 Bing: %v", err)
	}
	now = now.Add(1 * time.Minute)
	if _, err := svc.Execute(context.Background(), Request{Query: "测试"}); err != nil {
		t.Fatalf("应继续使用 Bing: %v", err)
	}
	if parallel.calls != 1 {
		t.Errorf("冷却期内不应再请求 Parallel, 实际 %d 次", parallel.calls)
	}
	if bing.calls != 2 {
		t.Errorf("Bing 调用次数 = %d, 期望 2", bing.calls)
	}
}

func TestTerminalFallbackStopsChain(t *testing.T) {
	exa := newPlatform("exa", 402, `{"error":"no credits"}`)
	defer exa.server.Close()
	svc, _ := newService(t, map[string]*fakePlatform{"exa": exa}, []string{"exa"}, nil)

	parallel := &fakeFallback{name: provider.ParallelName, err: provider.NewError(provider.ParallelName, provider.KindTransient, 500, "down")}
	bing := &fakeFallback{name: provider.BingName, err: provider.NewError(provider.BingName, provider.KindTransient, 500, "down")}
	after := &fakeFallback{name: "after", results: []provider.Result{{Title: "不该到这里", URL: "https://x.example"}}}
	svc.WithFallbackOptions(parallel, 10*time.Minute, false)
	svc.WithFallbackOptions(bing, 10*time.Minute, true)
	svc.WithFallbackOptions(after, 10*time.Minute, false)

	_, err := svc.Execute(context.Background(), Request{Query: "测试"})
	if err == nil {
		t.Fatal("全部失败应返回错误")
	}
	if after.calls != 0 {
		t.Errorf("终止通道失败后不应继续, 实际调用了 %d 次", after.calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, provider.ParallelName) || !strings.Contains(msg, provider.BingName) {
		t.Errorf("错误信息应包含各通道原因, 实际 %q", msg)
	}
}
