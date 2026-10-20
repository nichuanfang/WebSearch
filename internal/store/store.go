// Package store 提供基于 SQLite 的额度账本。
//
// 并发模型：WAL 模式下读并发写，写操作通过单一写连接串行化，
// 配合 busy_timeout 避免 "database is locked"。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// State 表示平台在当前周期的状态。
type State string

const (
	// StateOK 正常可用。
	StateOK State = "ok"
	// StateExhausted 额度耗尽，等下个周期自动恢复。
	StateExhausted State = "exhausted"
	// StateCooldown 速率限制冷却中。
	StateCooldown State = "cooldown"
	// StateAuthError 鉴权错误，需人工干预。
	StateAuthError State = "auth_error"
)

// Quota 是一行额度账本记录。
type Quota struct {
	Provider        string
	PeriodKey       string
	PeriodStart     time.Time
	PeriodEnd       time.Time
	LocalUsed       int
	ServerUsed      float64
	HasServerUsed   bool
	ServerLimit     float64
	HasServerLimit  bool
	ServerResetAt   time.Time
	ServerFetchedAt time.Time
	AnchorDate      time.Time
	State           State
	CooldownUntil   time.Time
	LastError       string
}

// LogEntry 是一条调用日志。
type LogEntry struct {
	Provider   string
	QueryHash  string
	NumResults int
	CostUnits  float64
	CostUSD    float64
	HasCostUSD bool
	LatencyMS  int
	Status     string
	ErrorKind  string
}

// Store 是 SQLite 账本。
type Store struct {
	db      *sql.DB
	writeDB *sql.DB
}

// Open 打开（必要时创建）数据库文件并初始化表结构。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	// 写连接限制为 1：SQLite 不支持多写者，串行化写事务最稳。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("打开写连接: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(time.Hour)

	s := &Store{db: db, writeDB: writeDB}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

const schemaSQL = `CREATE TABLE IF NOT EXISTS provider_quota (
  provider          TEXT NOT NULL,
  period_key        TEXT NOT NULL,
  period_start      TIMESTAMP NOT NULL,
  period_end        TIMESTAMP NOT NULL,
  local_used        INTEGER NOT NULL DEFAULT 0,
  server_used       REAL,
  server_limit      REAL,
  server_reset_at   TIMESTAMP,
  server_fetched_at TIMESTAMP,
  anchor_date       TIMESTAMP,
  state             TEXT NOT NULL DEFAULT 'ok',
  cooldown_until    TIMESTAMP,
  last_error        TEXT,
  PRIMARY KEY (provider, period_key)
);
CREATE TABLE IF NOT EXISTS search_log (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           TIMESTAMP NOT NULL,
  provider     TEXT NOT NULL,
  query_hash   TEXT NOT NULL,
  num_results  INTEGER NOT NULL,
  cost_units   REAL NOT NULL,
  cost_usd     REAL,
  latency_ms   INTEGER NOT NULL,
  status       TEXT NOT NULL,
  error_kind   TEXT
);
CREATE INDEX IF NOT EXISTS idx_search_log_ts ON search_log(ts);
CREATE INDEX IF NOT EXISTS idx_search_log_provider ON search_log(provider);`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("初始化表结构: %w", err)
	}
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	var firstErr error
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			firstErr = err
		}
	}
	if s.writeDB != nil {
		if err := s.writeDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ErrInsufficientQuota 表示额度不足。
var ErrInsufficientQuota = errors.New("额度不足")

// Reserve 在事务中预扣 units 个 credit，确保并发下不会超额。
// 额度不足时返回 ErrInsufficientQuota 且不产生任何写入。
func (s *Store) Reserve(ctx context.Context, provider, periodKey string, periodStart, periodEnd time.Time, limit int, units float64) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启预扣事务: %w", err)
	}
	defer tx.Rollback()

	var used int
	err = tx.QueryRowContext(ctx,
		`SELECT local_used FROM provider_quota WHERE provider = ? AND period_key = ?`,
		provider, periodKey).Scan(&used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		used = 0
	case err != nil:
		return fmt.Errorf("读取额度: %w", err)
	}
	if limit > 0 && float64(used)+units > float64(limit) {
		return fmt.Errorf("%w: 已用 %d，本地上限 %d，本次需要 %.0f", ErrInsufficientQuota, used, limit, units)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO provider_quota (provider, period_key, period_start, period_end, local_used, state)
VALUES (?, ?, ?, ?, ?, 'ok')
ON CONFLICT(provider, period_key) DO UPDATE SET local_used = local_used + excluded.local_used`,
		provider, periodKey, periodStart, periodEnd, int(units))
	if err != nil {
		return fmt.Errorf("写入预扣: %w", err)
	}
	return tx.Commit()
}

// Refund 退还一次失败的预扣。
func (s *Store) Refund(ctx context.Context, provider, periodKey string, units float64) error {
	_, err := s.writeDB.ExecContext(ctx,
		`UPDATE provider_quota SET local_used = MAX(0, local_used - ?) WHERE provider = ? AND period_key = ?`,
		int(units), provider, periodKey)
	if err != nil {
		return fmt.Errorf("退还额度: %w", err)
	}
	return nil
}

// SetState 更新平台状态与冷却时间。
func (s *Store) SetState(ctx context.Context, provider, periodKey string, state State, cooldownUntil time.Time, lastErr string) error {
	var cd any
	if !cooldownUntil.IsZero() {
		cd = cooldownUntil
	}
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO provider_quota (provider, period_key, period_start, period_end, local_used, state, cooldown_until, last_error)
VALUES (?, ?, ?, ?, 0, ?, ?, ?)
ON CONFLICT(provider, period_key) DO UPDATE SET
  state = excluded.state,
  cooldown_until = excluded.cooldown_until,
  last_error = excluded.last_error`,
		provider, periodKey, time.Now(), time.Now(), string(state), cd, lastErr)
	if err != nil {
		return fmt.Errorf("更新平台状态: %w", err)
	}
	return nil
}

// UpdateServerTruth 记录服务端上报的额度真值。
// serverLimit 为 0 表示服务端不限量；serverUsed 为负表示未知。
func (s *Store) UpdateServerTruth(ctx context.Context, provider, periodKey string, serverUsed, serverLimit float64, resetAt, fetchedAt time.Time) error {
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO provider_quota (provider, period_key, period_start, period_end, local_used, state,
  server_used, server_limit, server_reset_at, server_fetched_at)
VALUES (?, ?, ?, ?, 0, 'ok', ?, ?, ?, ?)
ON CONFLICT(provider, period_key) DO UPDATE SET
  server_used = excluded.server_used,
  server_limit = excluded.server_limit,
  server_reset_at = excluded.server_reset_at,
  server_fetched_at = excluded.server_fetched_at`,
		provider, periodKey, time.Now(), time.Now(), serverUsed, serverLimit, resetAt, fetchedAt)
	if err != nil {
		return fmt.Errorf("更新服务端额度: %w", err)
	}
	return nil
}

// GetQuota 读取指定周期内的额度记录。
func (s *Store) GetQuota(ctx context.Context, provider, periodKey string) (*Quota, error) {
	var q Quota
	var serverUsed, serverLimit sql.NullFloat64
	var serverReset, serverFetched, cooldown sql.NullTime
	var anchor sql.NullTime
	var lastError sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT provider, period_key, period_start, period_end, local_used,
       server_used, server_limit, server_reset_at, server_fetched_at, anchor_date,
       state, cooldown_until, last_error
FROM provider_quota WHERE provider = ? AND period_key = ?`,
		provider, periodKey).Scan(
		&q.Provider, &q.PeriodKey, &q.PeriodStart, &q.PeriodEnd, &q.LocalUsed,
		&serverUsed, &serverLimit, &serverReset, &serverFetched, &anchor,
		&q.State, &cooldown, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取额度记录: %w", err)
	}
	if serverUsed.Valid {
		q.ServerUsed, q.HasServerUsed = serverUsed.Float64, true
	}
	if serverLimit.Valid {
		q.ServerLimit, q.HasServerLimit = serverLimit.Float64, true
	}
	if serverReset.Valid {
		q.ServerResetAt = serverReset.Time
	}
	if serverFetched.Valid {
		q.ServerFetchedAt = serverFetched.Time
	}
	if anchor.Valid {
		q.AnchorDate = anchor.Time
	}
	if cooldown.Valid {
		q.CooldownUntil = cooldown.Time
	}
	if lastError.Valid {
		q.LastError = lastError.String
	}
	return &q, nil
}

// SetAnchorDate 记录滚动周期的锚点日。
func (s *Store) SetAnchorDate(ctx context.Context, provider, periodKey string, anchor time.Time) error {
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO provider_quota (provider, period_key, period_start, period_end, local_used, state, anchor_date)
VALUES (?, ?, ?, ?, 0, 'ok', ?)
ON CONFLICT(provider, period_key) DO UPDATE SET anchor_date = excluded.anchor_date`,
		provider, periodKey, time.Now(), time.Now(), anchor)
	if err != nil {
		return fmt.Errorf("记录锚点日: %w", err)
	}
	return nil
}

// ListAnchors 返回某平台所有已记录的锚点日，按时间升序排列。
// 首个元素即最初的锚点，用于推算当前滚动周期。
func (s *Store) ListAnchors(ctx context.Context, provider string) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT anchor_date FROM provider_quota
		 WHERE provider = ? AND anchor_date IS NOT NULL
		 ORDER BY anchor_date ASC`, provider)
	if err != nil {
		return nil, fmt.Errorf("读取锚点日: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("读取锚点日: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AppendLog 写入一条调用日志。
func (s *Store) AppendLog(ctx context.Context, e LogEntry) error {
	var costUSD any
	if e.HasCostUSD {
		costUSD = e.CostUSD
	}
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO search_log (ts, provider, query_hash, num_results, cost_units, cost_usd, latency_ms, status, error_kind)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now(), e.Provider, e.QueryHash, e.NumResults, e.CostUnits, costUSD, e.LatencyMS, e.Status, e.ErrorKind)
	if err != nil {
		return fmt.Errorf("写入调用日志: %w", err)
	}
	return nil
}

// ProviderStats 是某平台的聚合统计。
type ProviderStats struct {
	Provider  string
	Calls     int
	Successes int
	Failures  int
	CostUnits float64
	CostUSD   float64
}

// Stats 汇总自指定时刻以来的调用统计。
func (s *Store) Stats(ctx context.Context, since time.Time) ([]ProviderStats, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT provider,
       COUNT(*),
       SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status != 'ok' THEN 1 ELSE 0 END),
       COALESCE(SUM(cost_units), 0),
       COALESCE(SUM(cost_usd), 0)
FROM search_log WHERE ts >= ? GROUP BY provider`, since)
	if err != nil {
		return nil, fmt.Errorf("汇总统计: %w", err)
	}
	defer rows.Close()
	var out []ProviderStats
	for rows.Next() {
		var st ProviderStats
		if err := rows.Scan(&st.Provider, &st.Calls, &st.Successes, &st.Failures, &st.CostUnits, &st.CostUSD); err != nil {
			return nil, fmt.Errorf("读取统计: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
