package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	log "github.com/sirupsen/logrus"
	"github.com/therealtinhtute/llmhub/internal/runtimecontrol"
	cliproxyauth "github.com/therealtinhtute/llmhub/sdk/cliproxy/auth"
)

const (
	defaultConfigTable = "config_store"
	defaultAuthTable   = "auth_store"
	defaultUsageTable  = "usage_events"
	defaultConfigKey   = "config"
	authRuntimeKey     = "_llmhub_runtime"
)

// PostgresStoreConfig captures configuration required to initialize a Postgres-backed store.
type PostgresStoreConfig struct {
	DSN         string
	Schema      string
	ConfigTable string
	AuthTable   string
	UsageTable  string
	SpoolDir    string
}

// PostgresStore persists runtime config, auth records, and recent usage directly in PostgreSQL.
type PostgresStore struct {
	db *sql.DB

	cfg PostgresStoreConfig

	mu            sync.Mutex
	cooldownStore *postgresCooldownStateStore

	// Revision-keyed read caches. The storage watcher calls ObserveRevisions
	// every poll with the probe's revisions; a mismatch invalidates the entry.
	// Local writes update entries write-through so the writing process never
	// serves its own pre-write value.
	cacheMu     sync.RWMutex
	configCache struct {
		version int64
		content []byte
		valid   bool
	}
	rtCache struct {
		revision int64
		settings runtimecontrol.Settings
		valid    bool
	}
}

// ObserveRevisions feeds the storage watcher's probe results back into the
// read caches: a revision produced elsewhere (another instance, a management
// write on a different process) invalidates the matching cached value so the
// next read refetches. Staleness bound: one watcher poll interval.
func (s *PostgresStore) ObserveRevisions(configVersion, runtimeRevision int64) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.configCache.valid && s.configCache.version != configVersion {
		s.configCache.valid = false
	}
	if s.rtCache.valid && s.rtCache.revision != runtimeRevision {
		s.rtCache.valid = false
	}
}

type ConfigSnapshot struct {
	Content []byte
	Version int64
}

type AuthSnapshot struct {
	IDs       []string
	Count     int64
	MaxUpdate time.Time
}

// NewPostgresStore establishes a connection to PostgreSQL.
func NewPostgresStore(ctx context.Context, cfg PostgresStoreConfig) (*PostgresStore, error) {
	trimmedDSN := strings.TrimSpace(cfg.DSN)
	if trimmedDSN == "" {
		return nil, fmt.Errorf("postgres store: DSN is required")
	}
	cfg.DSN = trimmedDSN
	if cfg.ConfigTable == "" {
		cfg.ConfigTable = defaultConfigTable
	}
	if cfg.AuthTable == "" {
		cfg.AuthTable = defaultAuthTable
	}
	if cfg.UsageTable == "" {
		cfg.UsageTable = defaultUsageTable
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres store: open database connection: %w", err)
	}
	// Bound the pool for remote deployments: idle connections past
	// ConnMaxIdleTime are closed instead of going stale against NAT/pooler
	// timeouts, and MaxIdleConns keeps a warm set above the default of 2 so
	// bursty management reads do not pay a fresh TCP+TLS+auth handshake.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(10 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, postgresPingError(cfg.DSN, err)
	}

	store := &PostgresStore{
		db:  db,
		cfg: cfg,
	}
	store.cooldownStore = &postgresCooldownStateStore{store: store}
	return store, nil
}

func postgresPingError(dsn string, err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	dsnLower := strings.ToLower(dsn)
	if strings.Contains(dsnLower, ".supabase.co") &&
		strings.Contains(dsnLower, "db.") &&
		(strings.Contains(message, "no route to host") || strings.Contains(message, "network is unreachable")) {
		return fmt.Errorf("postgres store: ping database: %w; Supabase direct database hosts often require IPv6. Use the Supabase session-mode pooler DSN, for example postgres://postgres.<project-ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require (transaction mode :6543 does not support the session advisory locks used by quota collection)", err)
	}
	return fmt.Errorf("postgres store: ping database: %w", err)
}

// Close releases the underlying database connection.
func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnsureSchema creates the required tables and indexes.
func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store: not initialized")
	}
	if schema := strings.TrimSpace(s.cfg.Schema); schema != "" {
		query := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdentifier(schema))
		if _, err := s.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("postgres store: create schema: %w", err)
		}
	}

	configTable := s.fullTableName(s.cfg.ConfigTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			version BIGINT NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, configTable)); err != nil {
		return fmt.Errorf("postgres store: create config table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1", configTable)); err != nil {
		return fmt.Errorf("postgres store: add config version column: %w", err)
	}

	authTable := s.fullTableName(s.cfg.AuthTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL DEFAULT 'unknown',
			content JSONB NOT NULL,
			revision BIGINT NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, authTable)); err != nil {
		return fmt.Errorf("postgres store: create auth table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'unknown'", authTable)); err != nil {
		return fmt.Errorf("postgres store: add auth provider column: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1", authTable)); err != nil {
		return fmt.Errorf("postgres store: add auth revision column: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (provider)", s.indexName("auth_store_provider_idx"), authTable)); err != nil {
		return fmt.Errorf("postgres store: create auth provider index: %w", err)
	}

	usageTable := s.fullTableName(s.cfg.UsageTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id BIGSERIAL PRIMARY KEY,
			payload JSONB NOT NULL,
			requested_at TIMESTAMPTZ NOT NULL,
			popped_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, usageTable)); err != nil {
		return fmt.Errorf("postgres store: create usage table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (popped_at, requested_at, id)", s.indexName("usage_events_pop_idx"), usageTable)); err != nil {
		return fmt.Errorf("postgres store: create usage pop index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (created_at)", s.indexName("usage_events_created_idx"), usageTable)); err != nil {
		return fmt.Errorf("postgres store: create usage created index: %w", err)
	}
	if err := s.ensureQuotaAlertSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureRuntimeControlSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureCooldownSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureNativeProviderSchema(ctx); err != nil {
		return err
	}
	return nil
}

// ImportAuthFromDirectory imports auth JSON files from a legacy local directory.
func (s *PostgresStore) ImportAuthFromDirectory(ctx context.Context, authDir string, overwrite bool) (int, int, error) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("postgres store: read auth dir: %w", err)
	}
	imported := 0
	skipped := 0
	for _, entry := range entries {
		if entry == nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		data, errRead := os.ReadFile(path)
		if errRead != nil || len(data) == 0 {
			continue
		}
		id := entry.Name()
		if !overwrite {
			exists, errExists := s.authExists(ctx, id)
			if errExists != nil {
				return imported, skipped, errExists
			}
			if exists {
				skipped++
				continue
			}
		}
		if errSave := s.saveAuthPayload(ctx, id, data, time.Now()); errSave != nil {
			return imported, skipped, errSave
		}
		imported++
	}
	return imported, skipped, nil
}

func (s *PostgresStore) authExists(ctx context.Context, id string) (bool, error) {
	query := fmt.Sprintf("SELECT 1 FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	var exists int
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("postgres store: check auth exists: %w", err)
	}
	return true, nil
}

// PathlessAuthStore marks this backend as not requiring filesystem auth paths.
func (s *PostgresStore) PathlessAuthStore() bool { return true }

var _ cliproxyauth.RevisionedStore = (*PostgresStore)(nil)

func (s *PostgresStore) SetBaseDir(string) {}

// InitializeConfig saves config only when the Postgres config row is still empty.
func (s *PostgresStore) InitializeConfig(ctx context.Context, data []byte) (bool, int64, error) {
	if err := s.EnsureSchema(ctx); err != nil {
		return false, 0, err
	}
	version, err := s.CurrentVersion(ctx)
	if err != nil {
		return false, 0, err
	}
	if version > 0 {
		return false, version, nil
	}
	version, err = s.SaveConfig(ctx, data)
	if err != nil {
		return false, 0, err
	}
	return true, version, nil
}

// LoadConfig returns the current config bytes and version.
func (s *PostgresStore) LoadConfig(ctx context.Context) (*ConfigSnapshot, error) {
	query := fmt.Sprintf("SELECT content, version FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var content string
	var version int64
	if err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("postgres store: load config: %w", err)
	}
	return &ConfigSnapshot{Content: []byte(normalizeLineEndings(content)), Version: version}, nil
}

// LoadConfigBytes returns the current config payload for management handlers.
// The payload is cached keyed by config version; the watcher probe invalidates
// on remote change, SaveConfig writes through on local change.
func (s *PostgresStore) LoadConfigBytes(ctx context.Context) ([]byte, error) {
	s.cacheMu.RLock()
	if s.configCache.valid {
		content := append([]byte(nil), s.configCache.content...)
		s.cacheMu.RUnlock()
		return content, nil
	}
	s.cacheMu.RUnlock()

	snapshot, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.configCache.version = snapshot.Version
	s.configCache.content = append([]byte(nil), snapshot.Content...)
	s.configCache.valid = true
	s.cacheMu.Unlock()
	return append([]byte(nil), snapshot.Content...), nil
}

// SaveConfig stores config bytes and increments the version on updates.
func (s *PostgresStore) SaveConfig(ctx context.Context, data []byte) (int64, error) {
	normalized := normalizeLineEndings(string(data))
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, version, created_at, updated_at)
		VALUES ($1, $2, 1, NOW(), NOW())
		ON CONFLICT (id)
		DO UPDATE SET content = EXCLUDED.content, version = (SELECT version + 1 FROM %s WHERE id = $1), updated_at = NOW()
		RETURNING version
	`, s.fullTableName(s.cfg.ConfigTable), s.fullTableName(s.cfg.ConfigTable))
	var version int64
	if err := s.db.QueryRowContext(ctx, query, defaultConfigKey, normalized).Scan(&version); err != nil {
		return 0, fmt.Errorf("postgres store: save config: %w", err)
	}
	s.cacheMu.Lock()
	s.configCache.version = version
	s.configCache.content = append([]byte(nil), normalized...)
	s.configCache.valid = true
	s.cacheMu.Unlock()
	return version, nil
}

// CurrentVersion returns the config row version, or zero when no config exists.
func (s *PostgresStore) CurrentVersion(ctx context.Context) (int64, error) {
	query := fmt.Sprintf("SELECT version FROM %s WHERE id = $1", s.fullTableName(s.cfg.ConfigTable))
	var version int64
	if err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("postgres store: current config version: %w", err)
	}
	return version, nil
}

// Save persists an auth record directly to PostgreSQL.
func (s *PostgresStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("postgres store: auth is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	id := authRowKey(auth)
	if id == "" {
		return "", fmt.Errorf("postgres store: auth id is empty")
	}
	auth.ID = id
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = id
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = auth.Disabled
	raw, err := s.authPayload(auth)
	if err != nil {
		return "", err
	}
	if err = s.saveAuthPayload(ctx, id, raw, auth.UpdatedAt); err != nil {
		return "", err
	}
	return id, nil
}

// CountAuths returns the number of auth records without reading content payloads.
func (s *PostgresStore) CountAuths(ctx context.Context) (int, error) {
	query := fmt.Sprintf("SELECT count(*) FROM %s", s.fullTableName(s.cfg.AuthTable))
	var count int
	if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres store: count auths: %w", err)
	}
	return count, nil
}

// List enumerates all auth records stored in PostgreSQL.
func (s *PostgresStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, provider, content, created_at, updated_at FROM %s ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list auth: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id        string
			provider  string
			payload   []byte
			createdAt time.Time
			updatedAt time.Time
		)
		if err = rows.Scan(&id, &provider, &payload, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres store: scan auth row: %w", err)
		}
		auth, errBuild := authFromPayload(id, provider, payload, createdAt, updatedAt)
		if errBuild != nil {
			log.WithError(errBuild).Warnf("postgres store: skipping auth %s", id)
			continue
		}
		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// Delete removes the auth record identified by id.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("postgres store: id is empty")
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("postgres store: delete auth record: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		base := filepath.Base(id)
		if base != id && base != "." && base != "" {
			_, err = s.db.ExecContext(ctx, query, base)
			if err != nil {
				return fmt.Errorf("postgres store: delete auth record by base name: %w", err)
			}
		}
	}
	return nil
}

// LoadAuthContent returns the raw JSON payload stored for an auth record.
func (s *PostgresStore) LoadAuthContent(ctx context.Context, id string) ([]byte, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("postgres store: id is empty")
	}
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	var payload []byte
	err := s.db.QueryRowContext(ctx, query, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		base := filepath.Base(id)
		if base != id && base != "." && base != "" {
			err = s.db.QueryRowContext(ctx, query, base).Scan(&payload)
		}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("postgres store: load auth content: %w", err)
	}
	return append([]byte(nil), payload...), nil
}

// AuthSnapshot returns a lightweight change token for polling.
func (s *PostgresStore) AuthSnapshot(ctx context.Context) (AuthSnapshot, error) {
	query := fmt.Sprintf("SELECT id, updated_at FROM %s ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return AuthSnapshot{}, fmt.Errorf("postgres store: auth snapshot: %w", err)
	}
	defer rows.Close()
	var snapshot AuthSnapshot
	for rows.Next() {
		var id string
		var updatedAt time.Time
		if err = rows.Scan(&id, &updatedAt); err != nil {
			return AuthSnapshot{}, fmt.Errorf("postgres store: scan auth snapshot: %w", err)
		}
		snapshot.IDs = append(snapshot.IDs, id)
		snapshot.Count++
		if updatedAt.After(snapshot.MaxUpdate) {
			snapshot.MaxUpdate = updatedAt
		}
	}
	if err = rows.Err(); err != nil {
		return AuthSnapshot{}, fmt.Errorf("postgres store: iterate auth snapshot: %w", err)
	}
	sort.Strings(snapshot.IDs)
	return snapshot, nil
}

// AuthVersion returns a stable token that changes when auth rows are added, updated, or deleted.
func (s *PostgresStore) AuthVersion(ctx context.Context) (string, error) {
	snapshot, err := s.AuthSnapshot(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%s:%s", snapshot.Count, snapshot.MaxUpdate.UTC().Format(time.RFC3339Nano), strings.Join(snapshot.IDs, ",")), nil
}

// StorageRevisions fetches every watcher-visible revision in a single
// round-trip so the storage watcher costs one small query per poll instead of
// three (and the auth probe returns a constant-size hash instead of the full
// ID list). All subqueries are NULL-tolerant: absent rows yield zero values.
func (s *PostgresStore) StorageRevisions(ctx context.Context) (configVersion int64, authCount int64, authMaxUpdate time.Time, authIDsHash string, runtimeRevision int64, nativeMaxUpdate time.Time, err error) {
	query := fmt.Sprintf(`SELECT
		(SELECT version FROM %s WHERE id = $1),
		(SELECT count(*) FROM %s),
		(SELECT max(updated_at) FROM %s),
		(SELECT COALESCE(md5(string_agg(id, ',' ORDER BY id)), '') FROM %s),
		(SELECT revision FROM %s WHERE id = $2),
		(SELECT max(updated_at) FROM %s)`,
		s.fullTableName(s.cfg.ConfigTable),
		s.fullTableName(s.cfg.AuthTable),
		s.fullTableName(s.cfg.AuthTable),
		s.fullTableName(s.cfg.AuthTable),
		s.fullTableName(runtimeControlSettingsTable),
		s.fullTableName(nativeProviderTable))
	var (
		cfgVer    sql.NullInt64
		aCount    int64
		aMax      sql.NullTime
		aHash     sql.NullString
		rtRev     sql.NullInt64
		nativeMax sql.NullTime
	)
	if err = s.db.QueryRowContext(ctx, query, defaultConfigKey, runtimeControlSettingsID).Scan(
		&cfgVer, &aCount, &aMax, &aHash, &rtRev, &nativeMax,
	); err != nil {
		return 0, 0, time.Time{}, "", 0, time.Time{}, fmt.Errorf("postgres store: storage revisions: %w", err)
	}
	return cfgVer.Int64, aCount, aMax.Time, aHash.String, rtRev.Int64, nativeMax.Time, nil
}

// AppendUsageBatch inserts multiple usage payloads in one round-trip,
// preserving (requested_at, id) pop order via unnest's ordinality-free bulk
// insert (ids stay insertion-ordered within the batch).
func (s *PostgresStore) AppendUsageBatch(ctx context.Context, payloads [][]byte, requestedAts []time.Time) error {
	if len(payloads) == 0 {
		return nil
	}
	if len(payloads) != len(requestedAts) {
		return fmt.Errorf("postgres store: append usage batch: payloads/requestedAts length mismatch (%d != %d)", len(payloads), len(requestedAts))
	}
	texts := make([]string, len(payloads))
	for i, payload := range payloads {
		texts[i] = string(payload)
	}
	query := fmt.Sprintf(`INSERT INTO %s (payload, requested_at)
		SELECT p::jsonb, ts FROM unnest($1::text[], $2::timestamptz[]) AS u(p, ts)`, s.fullTableName(s.cfg.UsageTable))
	if _, err := s.db.ExecContext(ctx, query, texts, requestedAts); err != nil {
		return fmt.Errorf("postgres store: append usage batch: %w", err)
	}
	return nil
}

// AppendUsage stores a recent management usage payload.
func (s *PostgresStore) AppendUsage(ctx context.Context, payload []byte, requestedAt time.Time) error {
	if len(payload) == 0 {
		return nil
	}
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	query := fmt.Sprintf("INSERT INTO %s (payload, requested_at) VALUES ($1, $2)", s.fullTableName(s.cfg.UsageTable))
	if _, err := s.db.ExecContext(ctx, query, json.RawMessage(payload), requestedAt); err != nil {
		return fmt.Errorf("postgres store: append usage: %w", err)
	}
	return nil
}

// PopUsage returns oldest unpopped usage payloads and marks them popped atomically.
func (s *PostgresStore) PopUsage(ctx context.Context, count int) ([][]byte, error) {
	if count <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres store: begin usage pop: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	selectQuery := fmt.Sprintf(`
		SELECT id, payload
		FROM %s
		WHERE popped_at IS NULL
		ORDER BY requested_at, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, s.fullTableName(s.cfg.UsageTable))
	rows, err := tx.QueryContext(ctx, selectQuery, count)
	if err != nil {
		return nil, fmt.Errorf("postgres store: pop usage: %w", err)
	}
	defer rows.Close()
	out := make([][]byte, 0, count)
	ids := make([]int64, 0, count)
	for rows.Next() {
		var id int64
		var payload []byte
		if err = rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("postgres store: scan usage: %w", err)
		}
		ids = append(ids, id)
		out = append(out, append([]byte(nil), payload...))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: iterate usage: %w", err)
	}
	if len(ids) > 0 {
		updateQuery := fmt.Sprintf("UPDATE %s SET popped_at = NOW() WHERE id = ANY($1)", s.fullTableName(s.cfg.UsageTable))
		if _, err = tx.ExecContext(ctx, updateQuery, ids); err != nil {
			return nil, fmt.Errorf("postgres store: mark usage popped: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres store: commit usage pop: %w", err)
	}
	tx = nil
	return out, nil
}

// PruneUsage removes usage rows older than the retention window.
func (s *PostgresStore) PruneUsage(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	seconds := int64(retention.Seconds())
	if seconds <= 0 {
		return nil
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE created_at < NOW() - ($1 * INTERVAL '1 second')", s.fullTableName(s.cfg.UsageTable))
	if _, err := s.db.ExecContext(ctx, query, seconds); err != nil {
		return fmt.Errorf("postgres store: prune usage: %w", err)
	}
	return nil
}

// PersistAuthFiles remains for old watcher persister compatibility. Postgres mode does not watch files.
func (s *PostgresStore) PersistAuthFiles(context.Context, string, ...string) error { return nil }

// PersistConfig remains for old watcher persister compatibility. Postgres mode writes config directly.
func (s *PostgresStore) PersistConfig(context.Context) error { return nil }

func (s *PostgresStore) authPayload(auth *cliproxyauth.Auth) ([]byte, error) {
	if auth.Storage != nil {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["disabled"] = auth.Disabled
		if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
			setter.SetMetadata(auth.Metadata)
		}
		tmp, err := os.CreateTemp("", "llmhub-pg-auth-*.json")
		if err != nil {
			return nil, fmt.Errorf("postgres store: create temp auth payload: %w", err)
		}
		tmpPath := tmp.Name()
		if errClose := tmp.Close(); errClose != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("postgres store: close temp auth payload: %w", errClose)
		}
		defer func() { _ = os.Remove(tmpPath) }()
		if errSave := auth.Storage.SaveTokenToFile(tmpPath); errSave != nil {
			return nil, errSave
		}
		data, errRead := os.ReadFile(tmpPath)
		if errRead != nil {
			return nil, fmt.Errorf("postgres store: read temp auth payload: %w", errRead)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("postgres store: storage payload for %s is not valid json", auth.ID)
		}
		var merged map[string]any
		if err := json.Unmarshal(data, &merged); err == nil {
			for key, value := range merged {
				if key != authRuntimeKey {
					auth.Metadata[key] = value
				}
			}
		}
	}
	metadata := make(map[string]any)
	for key, value := range auth.Metadata {
		if key == authRuntimeKey {
			continue
		}
		metadata[key] = value
	}
	if strings.TrimSpace(auth.Provider) != "" {
		if _, ok := metadata["type"]; !ok {
			metadata["type"] = auth.Provider
		}
	}
	runtime := authRuntime{
		ID:               auth.ID,
		Provider:         auth.Provider,
		Prefix:           auth.Prefix,
		FileName:         auth.FileName,
		Label:            auth.Label,
		Status:           auth.Status,
		StatusMessage:    auth.StatusMessage,
		Disabled:         auth.Disabled,
		Unavailable:      auth.Unavailable,
		ProxyURL:         auth.ProxyURL,
		Attributes:       auth.Attributes,
		Quota:            auth.Quota,
		LastError:        auth.LastError,
		CreatedAt:        auth.CreatedAt,
		UpdatedAt:        auth.UpdatedAt,
		LastRefreshedAt:  auth.LastRefreshedAt,
		NextRefreshAfter: auth.NextRefreshAfter,
		NextRetryAfter:   auth.NextRetryAfter,
		ModelStates:      auth.ModelStates,
	}
	metadata[authRuntimeKey] = runtime
	return json.Marshal(metadata)
}

func (s *PostgresStore) saveAuthPayload(ctx context.Context, id string, data []byte, updatedAt time.Time) error {
	_, err := s.upsertAuthPayload(ctx, id, data, updatedAt, 0)
	return err
}

// upsertAuthPayload writes an auth payload. expected == 0 is an unconditional
// upsert that still advances the revision, so unconditional writers invalidate
// in-flight expectations. expected > 0 is a compare-and-swap on the stored
// revision: a stale expectation returns ErrRevisionConflict, a missing record
// returns os.ErrNotExist.
func (s *PostgresStore) upsertAuthPayload(ctx context.Context, id string, data []byte, updatedAt time.Time, expected int64) (int64, error) {
	if !json.Valid(data) {
		return 0, fmt.Errorf("postgres store: auth content for %s is not valid json", id)
	}
	provider := providerFromPayload(data)
	if provider == "" {
		provider = "unknown"
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	table := s.fullTableName(s.cfg.AuthTable)
	if expected > 0 {
		query := fmt.Sprintf(`
			UPDATE %s AS auth_record
			SET provider = $2, content = $3, updated_at = $4, revision = auth_record.revision + 1
			WHERE id = $1 AND revision = $5
			RETURNING revision
		`, table)
		var revision int64
		err := s.db.QueryRowContext(ctx, query, id, provider, json.RawMessage(data), updatedAt, expected).Scan(&revision)
		if err == nil {
			return revision, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("postgres store: compare-and-swap auth record: %w", err)
		}
		current, errCurrent := s.AuthRevision(ctx, id)
		if errCurrent != nil {
			return 0, errCurrent
		}
		return current, fmt.Errorf("postgres store: auth %s revision %d: %w", id, current, cliproxyauth.ErrRevisionConflict)
	}
	query := fmt.Sprintf(`
		INSERT INTO %s AS auth_record (id, provider, content, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), $4)
		ON CONFLICT (id)
		DO UPDATE SET provider = EXCLUDED.provider, content = EXCLUDED.content, updated_at = EXCLUDED.updated_at, revision = auth_record.revision + 1
		RETURNING revision
	`, table)
	var revision int64
	if err := s.db.QueryRowContext(ctx, query, id, provider, json.RawMessage(data), updatedAt).Scan(&revision); err != nil {
		return 0, fmt.Errorf("postgres store: upsert auth record: %w", err)
	}
	return revision, nil
}

// AuthRevision returns the stored revision for id.
func (s *PostgresStore) AuthRevision(ctx context.Context, id string) (int64, error) {
	id = authRowKeyFromID(id)
	if id == "" {
		return 0, fmt.Errorf("postgres store: id is empty")
	}
	query := fmt.Sprintf("SELECT revision FROM %s WHERE id = $1", s.fullTableName(s.cfg.AuthTable))
	var revision int64
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("postgres store: auth revision for %s: %w", id, os.ErrNotExist)
		}
		return 0, fmt.Errorf("postgres store: auth revision: %w", err)
	}
	return revision, nil
}

// SaveIfRevision persists auth only when the stored revision still matches
// expected, returning the new revision.
func (s *PostgresStore) SaveIfRevision(ctx context.Context, auth *cliproxyauth.Auth, expected int64) (int64, error) {
	if auth == nil {
		return 0, fmt.Errorf("postgres store: auth is nil")
	}
	if expected <= 0 {
		return 0, fmt.Errorf("postgres store: expected auth revision must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	id := authRowKey(auth)
	if id == "" {
		return 0, fmt.Errorf("postgres store: auth id is empty")
	}
	auth.ID = id
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = id
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = auth.Disabled
	raw, err := s.authPayload(auth)
	if err != nil {
		return 0, err
	}
	return s.upsertAuthPayload(ctx, id, raw, auth.UpdatedAt, expected)
}

func (s *PostgresStore) authRowCount(ctx context.Context) (int64, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", s.fullTableName(s.cfg.AuthTable))
	var count int64
	if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres store: count auth rows: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) fullTableName(name string) string {
	if strings.TrimSpace(s.cfg.Schema) == "" {
		return quoteIdentifier(name)
	}
	return quoteIdentifier(s.cfg.Schema) + "." + quoteIdentifier(name)
}

func (s *PostgresStore) indexName(name string) string {
	if strings.TrimSpace(s.cfg.Schema) == "" {
		return quoteIdentifier(name)
	}
	return quoteIdentifier(s.cfg.Schema + "_" + name)
}

type authRuntime struct {
	ID               string                              `json:"id,omitempty"`
	Provider         string                              `json:"provider,omitempty"`
	Prefix           string                              `json:"prefix,omitempty"`
	FileName         string                              `json:"file_name,omitempty"`
	Label            string                              `json:"label,omitempty"`
	Status           cliproxyauth.Status                 `json:"status,omitempty"`
	StatusMessage    string                              `json:"status_message,omitempty"`
	Disabled         bool                                `json:"disabled,omitempty"`
	Unavailable      bool                                `json:"unavailable,omitempty"`
	ProxyURL         string                              `json:"proxy_url,omitempty"`
	Attributes       map[string]string                   `json:"attributes,omitempty"`
	Quota            cliproxyauth.QuotaState             `json:"quota,omitempty"`
	LastError        *cliproxyauth.Error                 `json:"last_error,omitempty"`
	CreatedAt        time.Time                           `json:"created_at,omitempty"`
	UpdatedAt        time.Time                           `json:"updated_at,omitempty"`
	LastRefreshedAt  time.Time                           `json:"last_refreshed_at,omitempty"`
	NextRefreshAfter time.Time                           `json:"next_refresh_after,omitempty"`
	NextRetryAfter   time.Time                           `json:"next_retry_after,omitempty"`
	ModelStates      map[string]*cliproxyauth.ModelState `json:"model_states,omitempty"`
}

func authFromPayload(id, provider string, payload []byte, createdAt, updatedAt time.Time) (*cliproxyauth.Auth, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	var runtime authRuntime
	if rawRuntime, ok := metadata[authRuntimeKey]; ok {
		raw, _ := json.Marshal(rawRuntime)
		_ = json.Unmarshal(raw, &runtime)
		delete(metadata, authRuntimeKey)
	}
	if provider == "" || provider == "unknown" {
		provider = strings.TrimSpace(valueAsString(metadata["type"]))
	}
	if provider == "" {
		provider = strings.TrimSpace(runtime.Provider)
	}
	if provider == "" {
		provider = "unknown"
	}
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if runtime.Status != "" {
		status = runtime.Status
	}
	if disabled || runtime.Disabled {
		disabled = true
		status = cliproxyauth.StatusDisabled
	}
	fileName := strings.TrimSpace(runtime.FileName)
	if fileName == "" {
		fileName = id
	}
	authID := strings.TrimSpace(runtime.ID)
	if authID == "" {
		authID = id
	}
	auth := &cliproxyauth.Auth{
		ID:               normalizeAuthID(authID),
		Provider:         provider,
		Prefix:           runtime.Prefix,
		FileName:         fileName,
		Label:            firstNonEmpty(runtime.Label, labelFor(metadata)),
		Status:           status,
		StatusMessage:    runtime.StatusMessage,
		Disabled:         disabled,
		Unavailable:      runtime.Unavailable,
		ProxyURL:         runtime.ProxyURL,
		Attributes:       cloneStringMap(runtime.Attributes),
		Metadata:         metadata,
		Quota:            runtime.Quota,
		LastError:        runtime.LastError,
		CreatedAt:        firstTime(runtime.CreatedAt, createdAt),
		UpdatedAt:        firstTime(runtime.UpdatedAt, updatedAt),
		LastRefreshedAt:  runtime.LastRefreshedAt,
		NextRefreshAfter: runtime.NextRefreshAfter,
		NextRetryAfter:   runtime.NextRetryAfter,
		ModelStates:      runtime.ModelStates,
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["source"] = "postgres"
	if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
		auth.Attributes["email"] = email
	}
	cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
	return auth, nil
}

// authRowKeyFromID normalises an auth id into the auth_store primary key.
// Every row is written through this function, which makes it the identity for
// stored keys: normalising an existing key returns it unchanged. Read and write
// paths must both use it, or one of them addresses a row nobody wrote.
func authRowKeyFromID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(trimmed))
}

// authRowKey derives the auth_store key of an auth record, preferring ID over
// FileName exactly like the write path always has.
func authRowKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if key := authRowKeyFromID(auth.ID); key != "" {
		return key
	}
	return authRowKeyFromID(auth.FileName)
}

func providerFromPayload(payload []byte) string {
	var metadata map[string]any
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return ""
	}
	if rawRuntime, ok := metadata[authRuntimeKey]; ok {
		raw, _ := json.Marshal(rawRuntime)
		var runtime authRuntime
		if json.Unmarshal(raw, &runtime) == nil && strings.TrimSpace(runtime.Provider) != "" {
			return strings.TrimSpace(runtime.Provider)
		}
	}
	return strings.TrimSpace(valueAsString(metadata["type"]))
}

func firstExistingPath(paths ...string) string {
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func quoteIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "\"", "\"\"")
	return "\"" + replaced + "\""
}

func valueAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v := strings.TrimSpace(valueAsString(metadata["label"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["email"])); v != "" {
		return v
	}
	if v := strings.TrimSpace(valueAsString(metadata["project_id"])); v != "" {
		return v
	}
	return ""
}

func normalizeAuthID(id string) string {
	return filepath.ToSlash(filepath.Clean(id))
}

func normalizeLineEndings(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
