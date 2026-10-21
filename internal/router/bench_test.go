package router

import (
	"context"
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

type benchProvider struct{ name string }

func (p *benchProvider) Name() string { return p.name }
func (p *benchProvider) CostOf(provider.Query) float64 {
	return 1
}
func (p *benchProvider) Search(context.Context, provider.Query) (*provider.Response, error) {
	return &provider.Response{Provider: p.name, Results: []provider.Result{
		{Title: "标题", URL: "https://example.com", Snippet: "摘要"},
	}}, nil
}

func newBenchRouter(b *testing.B, n int) *Router {
	b.Helper()
	s, err := store.Open(filepath.Join(b.TempDir(), "r.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })

	names := []string{"exa", "brave", "tavily"}
	targets := make([]Target, 0, n)
	for i := 0; i < n; i++ {
		targets = append(targets, Target{
			Provider: &benchProvider{name: names[i%len(names)]},
			HasKey:   true,
			Config: config.ProviderConfig{
				Enabled: true, MonthlyLimit: 10_000_000, PeriodModel: config.PeriodCalendarMonth,
			},
		})
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(targets, quota.NewManager(s, time.Now), s, log)
}

// BenchmarkRouterSearch 含预扣、闸门、日志的完整选路开销（不含网络）。
func BenchmarkRouterSearch(b *testing.B) {
	r := newBenchRouter(b, 3)
	ctx := context.Background()
	q := provider.Query{Text: "golang generics", NumResults: 5}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Search(ctx, q, 3); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRouterSearchParallel 并发搜索。
func BenchmarkRouterSearchParallel(b *testing.B) {
	r := newBenchRouter(b, 3)
	ctx := context.Background()
	q := provider.Query{Text: "golang generics", NumResults: 5}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := r.Search(ctx, q, 3); err != nil {
				b.Fatal(err)
			}
		}
	})
}
