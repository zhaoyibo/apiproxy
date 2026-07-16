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
	CallCount        int64  `json:"call_count"`
	FailCount        int64  `json:"fail_count"`
}

type UsageEntry struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheHitTokens   int64
	CostCNY          string // unit: yuan (元)
	CallCount        int64
	FailCount        int64
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
		INSERT INTO daily_stats (key_id, date, model, input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny, call_count, fail_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			input_tokens       = input_tokens       + VALUES(input_tokens),
			output_tokens      = output_tokens      + VALUES(output_tokens),
			cache_write_tokens = cache_write_tokens + VALUES(cache_write_tokens),
			cache_hit_tokens   = cache_hit_tokens   + VALUES(cache_hit_tokens),
			cost_cny           = cost_cny           + VALUES(cost_cny),
			call_count         = call_count         + VALUES(call_count),
			fail_count         = fail_count         + VALUES(fail_count)
	`, keyID, date, entry.Model,
		entry.InputTokens, entry.OutputTokens,
		entry.CacheWriteTokens, entry.CacheHitTokens,
		entry.CostCNY, entry.CallCount, entry.FailCount,
	)
	return err
}

func (s *Store) Query(ctx context.Context, keyID int64, startDate, endDate string) ([]DailyStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, key_id, DATE_FORMAT(date, '%Y-%m-%d'), model,
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny,
		       call_count, fail_count
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
			&s.CacheWriteTokens, &s.CacheHitTokens, &s.CostCNY,
			&s.CallCount, &s.FailCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// --- APIKey ---
// ParentID == -1 means root key; QuotaCNY == "-1" means unlimited.

type APIKey struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	KeyCode   string  `json:"key_code"`
	ParentID  int64   `json:"parent_id"`          // -1 = root key; for child keys = primary (highest-priority) root, routing真源是 key_parents
	QuotaCNY  string  `json:"quota_cny"`          // "-1" = unlimited; unit: yuan (元)
	UsedCNY   string  `json:"used_cny"`           // unit: yuan (元); from Redis, not stored in DB
	IsActive  bool    `json:"is_active"`
	CreatedAt string  `json:"created_at"`
	RootIDs   []int64 `json:"root_ids,omitempty"` // child key: bound root ids in priority order; not persisted on api_keys
	Exhausted bool    `json:"exhausted,omitempty"` // root key: current-month upstream exhaustion flag; not persisted
}

// ParentRef pairs a root key's id with its key_code, used to warm the proxy cache
// with an ordered failover list.
type ParentRef struct {
	RootID  int64
	KeyCode string
}

// KeyParent is one row of the key_parents join table (child↔root, ordered).
type KeyParent struct {
	ChildID  int64 `json:"child_id"`
	RootID   int64 `json:"root_id"`
	Priority int   `json:"priority"`
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

// DeleteKey removes a single key row and all of its bindings. It does NOT
// cascade to child keys: deleting a root key merely unbinds it from children
// (a child may still be bound to other roots). Deleting a child removes its
// bindings. The caller is responsible for invalidating affected Redis caches.
func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_parents WHERE child_id = ? OR root_id = ?`, id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetChildParents replaces a child key's bound roots with the given ordered list
// (index = priority) and syncs api_keys.parent_id to the primary (first) root
// for backward-compatible reads.
func (s *Store) SetChildParents(ctx context.Context, childID int64, rootIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_parents WHERE child_id = ?`, childID); err != nil {
		return err
	}
	for i, rootID := range rootIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO key_parents (child_id, root_id, priority) VALUES (?, ?, ?)`,
			childID, rootID, i,
		); err != nil {
			return err
		}
	}
	if len(rootIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET parent_id = ? WHERE id = ?`, rootIDs[0], childID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetRootChildren reconciles which child keys are bound to the given root:
// children in childIDs but not yet bound get this root appended at the lowest
// priority; children currently bound but absent from childIDs get it removed.
// Other roots' bindings and their order are left untouched. Each affected child's
// parent_id is re-synced to its remaining top-priority root. Returns the ids of
// children whose bindings changed (for cache invalidation).
func (s *Store) SetRootChildren(ctx context.Context, rootID int64, childIDs []int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `SELECT child_id FROM key_parents WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	current := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	desired := make(map[int64]bool, len(childIDs))
	for _, id := range childIDs {
		desired[id] = true
	}

	var affected []int64
	// Adds: append this root at the lowest priority for that child.
	for id := range desired {
		if current[id] {
			continue
		}
		var maxPr sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(priority) FROM key_parents WHERE child_id = ?`, id).Scan(&maxPr); err != nil {
			return nil, err
		}
		pr := 0
		if maxPr.Valid {
			pr = int(maxPr.Int64) + 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_parents (child_id, root_id, priority) VALUES (?, ?, ?)`, id, rootID, pr); err != nil {
			return nil, err
		}
		affected = append(affected, id)
	}
	// Removes.
	for id := range current {
		if desired[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM key_parents WHERE child_id = ? AND root_id = ?`, id, rootID); err != nil {
			return nil, err
		}
		affected = append(affected, id)
	}
	// Re-sync parent_id for affected children to their current top-priority root.
	for _, id := range affected {
		var top int64
		err := tx.QueryRowContext(ctx, `SELECT root_id FROM key_parents WHERE child_id = ? ORDER BY priority LIMIT 1`, id).Scan(&top)
		if err == sql.ErrNoRows {
			continue // no bindings left; leave parent_id (stays != -1 so it's still a child)
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET parent_id = ? WHERE id = ?`, top, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}

// GetChildParents returns the child's bound root ids ordered by priority.
func (s *Store) GetChildParents(ctx context.Context, childID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT root_id FROM key_parents WHERE child_id = ? ORDER BY priority`, childID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetChildParentRefs returns the child's bound roots (id + key_code) ordered by
// priority, skipping inactive roots. Used to warm the proxy failover cache.
func (s *Store) GetChildParentRefs(ctx context.Context, childID int64) ([]ParentRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.key_code
		FROM key_parents kp
		JOIN api_keys r ON r.id = kp.root_id
		WHERE kp.child_id = ? AND r.is_active = 1
		ORDER BY kp.priority
	`, childID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []ParentRef{}
	for rows.Next() {
		var ref ParentRef
		if err := rows.Scan(&ref.RootID, &ref.KeyCode); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ListChildIDsByRoot returns the ids of all child keys bound to the given root.
func (s *Store) ListChildIDsByRoot(ctx context.Context, rootID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT child_id FROM key_parents WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListAllKeyParents returns every child↔root binding (for config export).
func (s *Store) ListAllKeyParents(ctx context.Context) ([]KeyParent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT child_id, root_id, priority FROM key_parents ORDER BY child_id, priority`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []KeyParent{}
	for rows.Next() {
		var kp KeyParent
		if err := rows.Scan(&kp.ChildID, &kp.RootID, &kp.Priority); err != nil {
			return nil, err
		}
		result = append(result, kp)
	}
	return result, rows.Err()
}

// ListAllChildKeys returns every child key (parent_id != -1) as a flat list.
func (s *Store) ListAllChildKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_code, parent_id, quota_cny, is_active, DATE_FORMAT(created_at, '%Y-%m-%dT%H:%i:%sZ')
		FROM api_keys WHERE parent_id != -1 ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKeys(rows)
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
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny,
		       call_count, fail_count
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
			&d.CacheWriteTokens, &d.CacheHitTokens, &d.CostCNY,
			&d.CallCount, &d.FailCount); err != nil {
			return nil, err
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

func (s *Store) ImportAll(ctx context.Context, keys []APIKey, dailyStats []DailyStat, prices map[string][]ModelPrice, keyParents []KeyParent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear all tables — order matters due to logical dependencies.
	for _, tbl := range []string{"key_parents", "daily_stats", "api_keys", "model_prices"} {
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
			INSERT INTO daily_stats (id, key_id, date, model, input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny, call_count, fail_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, d.ID, d.KeyID, d.Date, d.Model,
			d.InputTokens, d.OutputTokens,
			d.CacheWriteTokens, d.CacheHitTokens, d.CostCNY,
			d.CallCount, d.FailCount); err != nil {
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

	// Insert key_parents bindings. If the export predates many-to-many (none
	// provided), backfill from each child key's legacy parent_id.
	if len(keyParents) == 0 {
		for _, k := range keys {
			if k.ParentID != -1 {
				keyParents = append(keyParents, KeyParent{ChildID: k.ID, RootID: k.ParentID, Priority: 0})
			}
		}
	}
	for _, kp := range keyParents {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO key_parents (child_id, root_id, priority) VALUES (?, ?, ?)
		`, kp.ChildID, kp.RootID, kp.Priority); err != nil {
			return fmt.Errorf("insert key_parent %d->%d: %w", kp.ChildID, kp.RootID, err)
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
		       input_tokens, output_tokens, cache_write_tokens, cache_hit_tokens, cost_cny,
		       call_count, fail_count
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
			&s.CacheWriteTokens, &s.CacheHitTokens, &s.CostCNY,
			&s.CallCount, &s.FailCount); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}
