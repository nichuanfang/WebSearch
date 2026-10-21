package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReserveAcrossStores 回归：多个 Store 共享同一数据库文件时，
// 并发预扣不能因为 SQLITE_BUSY 而失败，且总数必须精确。
func TestReserveAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	const stores = 4
	const perStore = 25

	handles := make([]*Store, 0, stores)
	for i := 0; i < stores; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("打开第 %d 个连接失败: %v", i, err)
		}
		handles = append(handles, s)
		t.Cleanup(func() { s.Close() })
	}

	ctx := context.Background()
	now := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, stores)
	for i := 0; i < stores; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perStore; j++ {
				if err := handles[i].Reserve(ctx, "exa", "2026-10", now, now, 10_000, 1); err != nil {
					errs[i] = err
					return
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("写连接 %d 失败: %v", i, err)
		}
	}
	q, err := handles[0].GetQuota(ctx, "exa", "2026-10")
	if err != nil {
		t.Fatalf("读取额度失败: %v", err)
	}
	if q == nil || q.LocalUsed != stores*perStore {
		t.Errorf("已用 = %v, 期望 %d", q, stores*perStore)
	}
}

// TestReserveRespectsLimitAcrossStores 回归：多进程共享库时，
// 全局上限依然不能被突破。
func TestReserveRespectsLimitAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	const stores = 4
	const limit = 20

	handles := make([]*Store, 0, stores)
	for i := 0; i < stores; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("打开第 %d 个连接失败: %v", i, err)
		}
		handles = append(handles, s)
		t.Cleanup(func() { s.Close() })
	}

	ctx := context.Background()
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < stores; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if err := handles[i].Reserve(ctx, "exa", "2026-10", now, now, limit, 1); err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()

	if ok != limit {
		t.Errorf("成功预扣 = %d, 期望精确等于上限 %d", ok, limit)
	}
	q, _ := handles[0].GetQuota(ctx, "exa", "2026-10")
	if q == nil || q.LocalUsed != limit {
		t.Errorf("已用 = %v, 期望 %d", q, limit)
	}
}
