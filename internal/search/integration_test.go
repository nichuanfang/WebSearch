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
