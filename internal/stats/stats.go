package stats

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type DailyStat struct {
	ID               int64  `json:"id"`
	KeyID            int64  `json:"key_id"`
	Date             string `json:"date"`
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CacheHitTokens   int64  `json:"cache_hit_tokens"`
	CostCNY          string `json:"cost_cny"` // unit: yuan (元), e.g. "0.0000000300"
}

type UsageEntry struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheHitTokens   int64
	CostCNY          string // unit: yuan (元)
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Record(ctx context.Context, keyID int64, entry UsageEntry) error {
	date := time.Now().Format("2006-01-02")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO daily_stats (key_id, date, model, input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			input_tokens       = input_tokens       + VALUES(input_tokens),
			output_tokens      = output_tokens      + VALUES(output_tokens),
			cache_write_tokens = cache_write_tokens + VALUES(cache_write_tokens),
			cache_hit_tokens   = cache_hit_tokens   + VALUES(cache_hit_tokens),
			cost_cny           = cost_cny           + VALUES(cost_cny)
	`, keyID, date, entry.Model,
		entry.InputTokens, entry.OutputTokens,
		entry.CacheWriteTokens, entry.CacheHitTokens,
		entry.CostCNY,
	)
	return err
}

func (s *Store) Query(ctx context.Context, keyID int64, startDate, endDate string) ([]DailyStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key_id, DATE_FORMAT(date, '%Y-%m-%d'), model,
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny
		FROM daily_stats
		WHERE key_id = ? AND date BETWEEN ? AND ?
		ORDER BY date DESC, model
	`, keyID, startDate, endDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []DailyStat{}
	for rows.Next() {
		var s DailyStat
		if err := rows.Scan(&s.ID, &s.KeyID, &s.Date, &s.Model,
			&s.InputTokens, &s.OutputTokens,
			&s.CacheWriteTokens, &s.CacheHitTokens, &s.CostCNY); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// --- APIKey ---
// ParentID == -1 means root key; QuotaCNY == "-1" means unlimited.

type APIKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	KeyCode   string `json:"key_code"`
	ParentID  int64  `json:"parent_id"`  // -1 = root key
	QuotaCNY  string `json:"quota_cny"`  // "-1" = unlimited; unit: yuan (元)
	UsedCNY   string `json:"used_cny"`   // unit: yuan (元); from Redis, not stored in DB
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) CreateKey(ctx context.Context, k *APIKey) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (name, key_code, parent_id, quota_cny, is_active)
		VALUES (?, ?, ?, ?, ?)
	`, k.Name, k.KeyCode, k.ParentID, k.QuotaCNY, k.IsActive)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	k.ID = id
	return nil
}

func (s *Store) GetKeyByID(ctx context.Context, id int64) (*APIKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `
		SELECT id, name, key_code, parent_id, quota_cny, is_active, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')
		FROM api_keys WHERE id = ?
	`, id))
}

func (s *Store) GetKeyByCode(ctx context.Context, keyCode string) (*APIKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `
		SELECT id, name, key_code, parent_id, quota_cny, is_active, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')
		FROM api_keys WHERE key_code = ?
	`, keyCode))
}

func (s *Store) UpdateKey(ctx context.Context, k *APIKey) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET name = ?, quota_cny = ?, is_active = ? WHERE id = ?
	`, k.Name, k.QuotaCNY, k.IsActive, k.ID)
	return err
}

func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ? OR parent_id = ?`, id, id)
	return err
}

func (s *Store) ListRootKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_code, parent_id, quota_cny, is_active, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')
		FROM api_keys WHERE parent_id = -1 ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKeys(rows)
}

func (s *Store) ListChildKeys(ctx context.Context, parentID int64) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_code, parent_id, quota_cny, is_active, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')
		FROM api_keys WHERE parent_id = ? ORDER BY created_at
	`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKeys(rows)
}

func scanKey(row *sql.Row) (*APIKey, error) {
	var k APIKey
	if err := row.Scan(&k.ID, &k.Name, &k.KeyCode, &k.ParentID, &k.QuotaCNY, &k.IsActive, &k.CreatedAt); err != nil {
		return nil, err
	}
	return &k, nil
}

func scanKeys(rows *sql.Rows) ([]APIKey, error) {
	result := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyCode, &k.ParentID, &k.QuotaCNY, &k.IsActive, &k.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, rows.Err()
}

// --- ModelPrice ---
// ContextMax == -1 means no upper limit.

type ModelPrice struct {
	ID            int64  `json:"id"`
	Model         string `json:"model"`
	ContextMin    int64  `json:"context_min"`
	ContextMax    int64  `json:"context_max"`     // -1 = no upper limit
	InputCNY      string `json:"input_cny"`       // unit: yuan/百万token, e.g. "0.3"
	OutputCNY     string `json:"output_cny"`
	CacheHitCNY   string `json:"cache_hit_cny"`
	CacheWriteCNY string `json:"cache_write_cny"`
}

func (s *Store) SetModelPrices(ctx context.Context, model string, prices []ModelPrice) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM model_prices WHERE model = ?`, model); err != nil {
		return err
	}
	for _, p := range prices {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_prices (model, context_min, context_max, input_cny, output_cny, cache_hit_cny, cache_write_cny)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, model, p.ContextMin, p.ContextMax, p.InputCNY, p.OutputCNY, p.CacheHitCNY, p.CacheWriteCNY); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetModelPrices(ctx context.Context, model string) ([]ModelPrice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model, context_min, context_max, input_cny, output_cny, cache_hit_cny, cache_write_cny
		FROM model_prices WHERE model = ?
		ORDER BY context_min DESC
	`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPrices(rows)
}

func (s *Store) GetAllModelPrices(ctx context.Context) (map[string][]ModelPrice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model, context_min, context_max, input_cny, output_cny, cache_hit_cny, cache_write_cny
		FROM model_prices
		ORDER BY model, context_min ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	prices, err := scanPrices(rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]ModelPrice)
	for _, p := range prices {
		result[p.Model] = append(result[p.Model], p)
	}
	return result, nil
}

func (s *Store) DeleteModelPrices(ctx context.Context, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_prices WHERE model = ?`, model)
	return err
}

// FindPrice returns the matching price tier for a model and total context length.
// Tiers are ordered by context_min DESC so the first match is the highest applicable tier.
// ContextMax == -1 means no upper limit.
func (s *Store) FindPrice(ctx context.Context, model string, contextLen int64) (*ModelPrice, error) {
	prices, err := s.GetModelPrices(ctx, model)
	if err != nil {
		return nil, err
	}
	for _, p := range prices {
		if contextLen >= p.ContextMin {
			if p.ContextMax == -1 || contextLen < p.ContextMax {
				pc := p
				return &pc, nil
			}
		}
	}
	return nil, fmt.Errorf("no price found for model %s context %d", model, contextLen)
}

func (s *Store) ListAllStats(ctx context.Context) ([]DailyStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key_id, DATE_FORMAT(date, '%Y-%m-%d'), model,
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny
		FROM daily_stats
		ORDER BY date, key_id, model
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []DailyStat{}
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.ID, &d.KeyID, &d.Date, &d.Model,
			&d.InputTokens, &d.OutputTokens,
			&d.CacheWriteTokens, &d.CacheHitTokens, &d.CostCNY); err != nil {
			return nil, err
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

func (s *Store) ImportAll(ctx context.Context, keys []APIKey, dailyStats []DailyStat, prices map[string][]ModelPrice) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear all tables — order matters due to logical dependencies.
	for _, tbl := range []string{"daily_stats", "api_keys", "model_prices"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
			return fmt.Errorf("clear %s: %w", tbl, err)
		}
	}

	// Insert api_keys preserving original ids (daily_stats.key_id references them).
	for _, k := range keys {
		createdAt, err := time.Parse(time.RFC3339, k.CreatedAt)
		if err != nil {
			// Fall back to the raw value and let MySQL handle it.
			createdAt = time.Time{}
		}
		var createdAtVal interface{}
		if createdAt.IsZero() {
			createdAtVal = k.CreatedAt
		} else {
			createdAtVal = createdAt.UTC().Format("2006-01-02 15:04:05")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO api_keys (id, name, key_code, parent_id, quota_cny, is_active, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, k.ID, k.Name, k.KeyCode, k.ParentID, k.QuotaCNY, k.IsActive, createdAtVal); err != nil {
			return fmt.Errorf("insert api_key %d: %w", k.ID, err)
		}
	}

	// Insert daily_stats preserving original ids and key_id references.
	for _, d := range dailyStats {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO daily_stats (id, key_id, date, model, input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, d.ID, d.KeyID, d.Date, d.Model,
			d.InputTokens, d.OutputTokens,
			d.CacheWriteTokens, d.CacheHitTokens, d.CostCNY); err != nil {
			return fmt.Errorf("insert daily_stat %d: %w", d.ID, err)
		}
	}

	// Insert model_prices.
	for _, tiers := range prices {
		for _, p := range tiers {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO model_prices (model, context_min, context_max, input_cny, output_cny, cache_hit_cny, cache_write_cny)
				VALUES (?, ?, ?, ?, ?, ?, ?)
			`, p.Model, p.ContextMin, p.ContextMax,
				p.InputCNY, p.OutputCNY, p.CacheHitCNY, p.CacheWriteCNY); err != nil {
				return fmt.Errorf("insert model_price %s: %w", p.Model, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Reset AUTO_INCREMENT counters so new rows don't clash with the imported IDs.
	// Must run outside the transaction because ALTER TABLE causes an implicit commit.
	for _, tbl := range []string{"api_keys", "daily_stats", "model_prices"} {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+tbl+" AUTO_INCREMENT = 1"); err != nil {
			return fmt.Errorf("reset auto_increment %s: %w", tbl, err)
		}
	}
	return nil
}

func scanPrices(rows *sql.Rows) ([]ModelPrice, error) {
	result := []ModelPrice{}
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.ID, &p.Model, &p.ContextMin, &p.ContextMax,
			&p.InputCNY, &p.OutputCNY, &p.CacheHitCNY, &p.CacheWriteCNY); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// --- Daily stats ---

func (s *Store) QueryByParent(ctx context.Context, childKeyIDs []int64, startDate, endDate string) ([]DailyStat, error) {
	if len(childKeyIDs) == 0 {
		return []DailyStat{}, nil
	}

	placeholders := strings.Repeat("?,", len(childKeyIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]interface{}, 0, len(childKeyIDs)+2)
	for _, id := range childKeyIDs {
		args = append(args, id)
	}
	args = append(args, startDate, endDate)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key_id, DATE_FORMAT(date, '%Y-%m-%d'), model,
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny
		FROM daily_stats
		WHERE key_id IN (`+placeholders+`) AND date BETWEEN ? AND ?
		ORDER BY date DESC, key_id, model
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []DailyStat{}
	for rows.Next() {
		var s DailyStat
		if err := rows.Scan(&s.ID, &s.KeyID, &s.Date, &s.Model,
			&s.InputTokens, &s.OutputTokens,
			&s.CacheWriteTokens, &s.CacheHitTokens, &s.CostCNY); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}
