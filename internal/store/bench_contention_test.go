package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// BenchmarkMultiStoreContention 模拟多个 websearch 进程共享同一个数据库文件。
// 每个 Store 有独立的写连接，SQLite 只允许单写者，其余靠 busy_timeout 等待。
func BenchmarkMultiStoreContention(b *testing.B) {
	for _, stores := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("stores=%d", stores), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "shared.db")
			handles := make([]*Store, 0, stores)
			for i := 0; i < stores; i++ {
				s, err := Open(path)
				if err != nil {
					b.Fatal(err)
				}
				handles = append(handles, s)
				b.Cleanup(func() { s.Close() })
			}
			ctx := context.Background()
			now := time.Now()

			b.ResetTimer()
			var wg sync.WaitGroup
			perStore := b.N / stores
			if perStore < 1 {
				perStore = 1
			}
			errs := make([]error, stores)
			for i := 0; i < stores; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := 0; j < perStore; j++ {
						if err := handles[i].Reserve(ctx, "exa", "2026-10", now, now, 1_000_000, 1); err != nil {
							errs[i] = err
							return
						}
					}
				}(i)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					b.Fatalf("并发写失败: %v", err)
				}
			}
			b.StopTimer()
			q, _ := handles[0].GetQuota(ctx, "exa", "2026-10")
			if q == nil || q.LocalUsed != stores*perStore {
				b.Fatalf("计数不一致: %+v", q)
			}
		})
	}
}

// BenchmarkReserveThenLog 模拟一次成功搜索的完整本地开销。
func BenchmarkReserveThenLog(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Reserve(ctx, "exa", "2026-10", now, now, 1_000_000, 1); err != nil {
			b.Fatal(err)
		}
		if err := s.AppendLog(ctx, LogEntry{
			Provider: "exa", QueryHash: "h", NumResults: 5,
			CostUnits: 1, LatencyMS: 100, Status: "ok",
		}); err != nil {
			b.Fatal(err)
		}
	}
}
