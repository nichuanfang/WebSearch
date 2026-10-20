package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReserveAndRefund(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.Reserve(ctx, "exa", "2026-10", now, now.AddDate(0, 1, 0), 100, 1); err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	q, err := s.GetQuota(ctx, "exa", "2026-10")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if q == nil || q.LocalUsed != 1 {
		t.Fatalf("预扣后已用量应为 1, 实际 %+v", q)
	}

	if err := s.Refund(ctx, "exa", "2026-10", 1); err != nil {
		t.Fatalf("退还失败: %v", err)
	}
	q, _ = s.GetQuota(ctx, "exa", "2026-10")
	if q.LocalUsed != 0 {
		t.Errorf("退还后已用量应为 0, 实际 %d", q.LocalUsed)
	}
}

func TestReserveRespectsLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.Reserve(ctx, "exa", "2026-10", now, now.AddDate(0, 1, 0), 2, 2); err != nil {
		t.Fatalf("首次预扣失败: %v", err)
	}
	// 上限 2，再预扣 1 应被拒绝。
	err := s.Reserve(ctx, "exa", "2026-10", now, now.AddDate(0, 1, 0), 2, 1)
	if err == nil {
		t.Fatal("超出上限的预扣应被拒绝")
	}
	// 失败后不应产生写入。
	q, _ := s.GetQuota(ctx, "exa", "2026-10")
	if q.LocalUsed != 2 {
		t.Errorf("被拒绝的预扣不应改变已用量, 实际 %d", q.LocalUsed)
	}
}

func TestConcurrentReserveNeverExceedsLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	const limit = 50
	const workers = 200

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Reserve(ctx, "brave", "p1", now, now.AddDate(0, 0, 31), limit, 1); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded != limit {
		t.Errorf("成功预扣次数 = %d, 期望恰好 %d（并发下不得超限）", succeeded, limit)
	}
	q, _ := s.GetQuota(ctx, "brave", "p1")
	if q.LocalUsed != limit {
		t.Errorf("最终已用量 = %d, 期望 %d", q.LocalUsed, limit)
	}
}

func TestRefundDoesNotGoNegative(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.Reserve(ctx, "tavily", "p1", now, now.AddDate(0, 1, 0), 100, 2); err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if err := s.Refund(ctx, "tavily", "p1", 5); err != nil {
		t.Fatalf("退还失败: %v", err)
	}
	q, _ := s.GetQuota(ctx, "tavily", "p1")
	if q.LocalUsed != 0 {
		t.Errorf("退还后已用量不应为负, 实际 %d", q.LocalUsed)
	}
}

func TestSeparatePeriodsAreIndependent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.Reserve(ctx, "exa", "2026-10", now, now.AddDate(0, 1, 0), 100, 3); err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if err := s.Reserve(ctx, "exa", "2026-11", now, now.AddDate(0, 1, 0), 100, 1); err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	q1, _ := s.GetQuota(ctx, "exa", "2026-10")
	q2, _ := s.GetQuota(ctx, "exa", "2026-11")
	if q1.LocalUsed != 3 || q2.LocalUsed != 1 {
		t.Errorf("跨周期记账应独立, 实际 10 月=%d 11 月=%d", q1.LocalUsed, q2.LocalUsed)
	}
}

func TestSetStateAndServerTruth(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.SetState(ctx, "exa", "p1", StateCooldown, now.Add(time.Minute), "限速"); err != nil {
		t.Fatalf("设置状态失败: %v", err)
	}
	if err := s.UpdateServerTruth(ctx, "exa", "p1", 5, 100, now.Add(time.Hour), now); err != nil {
		t.Fatalf("写入服务端额度失败: %v", err)
	}
	q, _ := s.GetQuota(ctx, "exa", "p1")
	if q.State != StateCooldown {
		t.Errorf("状态 = %q, 期望 cooldown", q.State)
	}
	if !q.HasServerUsed || q.ServerUsed != 5 {
		t.Errorf("服务端已用量 = %v, 期望 5", q.ServerUsed)
	}
	if q.LastError != "限速" {
		t.Errorf("最近错误 = %q, 期望 限速", q.LastError)
	}
}

func TestAppendLogAndStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.AppendLog(ctx, LogEntry{
		Provider: "exa", QueryHash: "abc", NumResults: 5, CostUnits: 1,
		CostUSD: 0.004, HasCostUSD: true, LatencyMS: 300, Status: "ok",
	}); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
	if err := s.AppendLog(ctx, LogEntry{
		Provider: "brave", QueryHash: "def", NumResults: 0, CostUnits: 1,
		LatencyMS: 100, Status: "error", ErrorKind: "quota_exhausted",
	}); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
	stats, err := s.Stats(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("统计应有 2 个平台, 实际 %d", len(stats))
	}
	for _, st := range stats {
		switch st.Provider {
		case "exa":
			if st.Successes != 1 || st.CostUSD != 0.004 {
				t.Errorf("exa 统计异常: %+v", st)
			}
		case "brave":
			if st.Failures != 1 {
				t.Errorf("brave 统计异常: %+v", st)
			}
		}
	}
}
