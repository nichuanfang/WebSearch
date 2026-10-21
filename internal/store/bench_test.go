package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("打开失败: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// BenchmarkReserve 单次预扣（事务 + 读 + 写 + 提交）。
func BenchmarkReserve(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Reserve(ctx, "exa", "2026-10", now, now, 1_000_000, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReserveParallel 并发预扣，验证单写连接下的吞吐。
func BenchmarkReserveParallel(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	now := time.Now()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.Reserve(ctx, "exa", "2026-10", now, now, 1_000_000, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGetQuota 单次额度读取。
func BenchmarkGetQuota(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	now := time.Now()
	_ = s.Reserve(ctx, "exa", "2026-10", now, now, 1000, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetQuota(ctx, "exa", "2026-10"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendLog 单次调用日志写入。
func BenchmarkAppendLog(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	entry := LogEntry{Provider: "exa", QueryHash: "abc123", NumResults: 5, CostUnits: 1, LatencyMS: 120, Status: "ok"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.AppendLog(ctx, entry); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListAnchors 锚点查询随周期增长的退化情况。
func BenchmarkListAnchors(b *testing.B) {
	for _, n := range []int{1, 12, 120} {
		b.Run(fmt.Sprintf("periods=%d", n), func(b *testing.B) {
			s := newBenchStore(b)
			ctx := context.Background()
			base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("p%d", i)
				if err := s.SetAnchorDate(ctx, "tavily", key, base.AddDate(0, i, 0)); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListAnchors(ctx, "tavily"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
