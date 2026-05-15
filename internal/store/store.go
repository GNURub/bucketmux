package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/gnurub/bucketmux/internal/domain"
)

type dialect string

const (
	dialectSQLite   dialect = "sqlite"
	dialectPostgres dialect = "postgres"
)

type Store struct {
	db      *sql.DB
	dialect dialect
}

func Open(path string) (*Store, error) {
	return OpenSQLite(path)
}

func OpenConfig(cfg config.StoreConfig, legacyDBPath string) (*Store, error) {
	if cfg.Kind == "" {
		cfg.Kind = "sqlite"
	}
	switch cfg.Kind {
	case "sqlite":
		path := cfg.SQLite.Path
		if path == "" {
			path = legacyDBPath
		}
		return OpenSQLite(path)
	case "postgres":
		return OpenPostgres(cfg.Postgres)
	default:
		return nil, fmt.Errorf("unknown store kind %q", cfg.Kind)
	}
}

func OpenSQLite(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db, dialect: dialectSQLite}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func OpenPostgres(cfg config.PostgresStoreConfig) (*Store, error) {
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	s := &Store{db: db, dialect: dialectPostgres}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(query), args...)
}

func (s *Store) rebind(query string) string {
	if s.dialect != dialectPostgres {
		return query
	}
	var builder strings.Builder
	builder.Grow(len(query) + 8)
	placeholder := 1
	for _, ch := range query {
		if ch == '?' {
			builder.WriteByte('$')
			builder.WriteString(fmt.Sprintf("%d", placeholder))
			placeholder++
			continue
		}
		builder.WriteRune(ch)
	}
	return builder.String()
}

func (s *Store) migrate(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS provider_accounts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  endpoint TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  bucket TEXT NOT NULL,
  access_key TEXT NOT NULL DEFAULT '',
  secret_encrypted TEXT NOT NULL DEFAULT '',
  capacity_bytes INTEGER NOT NULL DEFAULT 0,
  used_bytes INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 100,
  enabled INTEGER NOT NULL DEFAULT 1,
  settings_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS buckets (
  name TEXT PRIMARY KEY,
  replication_enabled INTEGER NOT NULL DEFAULT 0,
  replication_provider_ids_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS objects (
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  provider_account_id TEXT NOT NULL,
  remote_bucket TEXT NOT NULL,
  remote_key TEXT NOT NULL,
  size INTEGER NOT NULL,
  content_type TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  checksum_sha256 TEXT NOT NULL DEFAULT '',
  replica_status TEXT NOT NULL DEFAULT 'none',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (bucket, key),
  FOREIGN KEY (provider_account_id) REFERENCES provider_accounts(id)
);
CREATE INDEX IF NOT EXISTS idx_objects_bucket_key ON objects(bucket, key);
CREATE TABLE IF NOT EXISTS object_replicas (
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  provider_account_id TEXT NOT NULL,
  remote_bucket TEXT NOT NULL DEFAULT '',
  remote_key TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  etag TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (bucket, key, provider_account_id)
);
CREATE INDEX IF NOT EXISTS idx_object_replicas_bucket_key ON object_replicas(bucket, key);
CREATE TABLE IF NOT EXISTS multipart_uploads (
  upload_id TEXT PRIMARY KEY,
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS multipart_parts (
  upload_id TEXT NOT NULL,
  part_number INTEGER NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL,
  etag TEXT NOT NULL,
  checksum_sha256 TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (upload_id, part_number),
  FOREIGN KEY (upload_id) REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_multipart_parts_upload ON multipart_parts(upload_id, part_number);
CREATE TABLE IF NOT EXISTS hooks (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  url TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT 'POST',
  events_json TEXT NOT NULL DEFAULT '[]',
  headers_encrypted TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS hook_deliveries (
  id TEXT PRIMARY KEY,
  hook_id TEXT NOT NULL,
  event TEXT NOT NULL,
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  next_attempt_at TEXT NOT NULL,
  last_status_code INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (hook_id) REFERENCES hooks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_hook_deliveries_recent ON hook_deliveries(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hook_deliveries_pending ON hook_deliveries(status, next_attempt_at);
CREATE TABLE IF NOT EXISTS migration_jobs (
  id TEXT PRIMARY KEY,
  bucket TEXT NOT NULL,
  prefix TEXT NOT NULL DEFAULT '',
  source_provider_id TEXT NOT NULL,
  target_provider_id TEXT NOT NULL,
  mode TEXT NOT NULL,
  status TEXT NOT NULL,
  total_objects INTEGER NOT NULL DEFAULT 0,
  processed_objects INTEGER NOT NULL DEFAULT 0,
  succeeded_objects INTEGER NOT NULL DEFAULT 0,
  failed_objects INTEGER NOT NULL DEFAULT 0,
  current_key TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT '',
  finished_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_migration_jobs_recent ON migration_jobs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_migration_jobs_status ON migration_jobs(status, created_at);
`
	_, err := s.exec(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.addColumnIfMissing(ctx, "hooks", "headers_encrypted", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "buckets", "replication_provider_ids_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	return nil
}

func (s *Store) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	_, err := s.exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	if err == nil || strings.Contains(err.Error(), "duplicate column name") || strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return fmt.Errorf("add column %s.%s: %w", table, column, err)
}

func (s *Store) UpsertProvider(ctx context.Context, p domain.ProviderAccount) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	settings, err := json.Marshal(p.Settings)
	if err != nil {
		return fmt.Errorf("encode provider settings: %w", err)
	}
	_, err = s.exec(ctx, `
INSERT INTO provider_accounts (id, name, kind, endpoint, region, bucket, access_key, secret_encrypted, capacity_bytes, used_bytes, priority, enabled, settings_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name,
  kind=excluded.kind,
  endpoint=excluded.endpoint,
  region=excluded.region,
  bucket=excluded.bucket,
  access_key=excluded.access_key,
  secret_encrypted=excluded.secret_encrypted,
  capacity_bytes=excluded.capacity_bytes,
  priority=excluded.priority,
  enabled=excluded.enabled,
  settings_json=excluded.settings_json,
  updated_at=excluded.updated_at
`, p.ID, p.Name, string(p.Kind), p.Endpoint, p.Region, p.Bucket, p.AccessKey, p.SecretEncrypted, p.CapacityBytes, p.UsedBytes, p.Priority, boolToInt(p.Enabled), string(settings), p.CreatedAt.Format(time.RFC3339Nano), p.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert provider: %w", err)
	}
	return nil
}

func (s *Store) ListProviders(ctx context.Context, enabledOnly bool) ([]domain.ProviderAccount, error) {
	query := `SELECT id, name, kind, endpoint, region, bucket, access_key, secret_encrypted, capacity_bytes, used_bytes, priority, enabled, settings_json, created_at, updated_at FROM provider_accounts`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY priority ASC, id ASC`
	rows, err := s.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	var out []domain.ProviderAccount
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProvider(ctx context.Context, id string) (domain.ProviderAccount, error) {
	row := s.queryRow(ctx, `SELECT id, name, kind, endpoint, region, bucket, access_key, secret_encrypted, capacity_bytes, used_bytes, priority, enabled, settings_json, created_at, updated_at FROM provider_accounts WHERE id = ?`, id)
	p, err := scanProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ProviderAccount{}, ErrNotFound
		}
		return domain.ProviderAccount{}, err
	}
	return p, nil
}

func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM provider_accounts WHERE id = ?`, id)
	return err
}

func (s *Store) UpsertHook(ctx context.Context, hook domain.Hook) error {
	now := time.Now().UTC()
	if hook.CreatedAt.IsZero() {
		hook.CreatedAt = now
	}
	hook.UpdatedAt = now
	events, err := json.Marshal(hook.Events)
	if err != nil {
		return fmt.Errorf("encode hook events: %w", err)
	}
	_, err = s.exec(ctx, `
INSERT INTO hooks (id, name, kind, url, method, events_json, headers_encrypted, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name,
  kind=excluded.kind,
  url=excluded.url,
  method=excluded.method,
  events_json=excluded.events_json,
  headers_encrypted=excluded.headers_encrypted,
  enabled=excluded.enabled,
  updated_at=excluded.updated_at
`, hook.ID, hook.Name, string(hook.Kind), hook.URL, hook.Method, string(events), hook.HeadersEncrypted, boolToInt(hook.Enabled), hook.CreatedAt.Format(time.RFC3339Nano), hook.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert hook: %w", err)
	}
	return nil
}

func (s *Store) ListHooks(ctx context.Context, enabledOnly bool) ([]domain.Hook, error) {
	query := `SELECT id, name, kind, url, method, events_json, headers_encrypted, enabled, created_at, updated_at FROM hooks`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list hooks: %w", err)
	}
	defer rows.Close()

	var out []domain.Hook
	for rows.Next() {
		hook, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hook)
	}
	return out, rows.Err()
}

func (s *Store) GetHook(ctx context.Context, id string) (domain.Hook, error) {
	row := s.queryRow(ctx, `SELECT id, name, kind, url, method, events_json, headers_encrypted, enabled, created_at, updated_at FROM hooks WHERE id = ?`, id)
	hook, err := scanHook(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Hook{}, ErrNotFound
		}
		return domain.Hook{}, err
	}
	return hook, nil
}

func (s *Store) DeleteHook(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM hooks WHERE id = ?`, id)
	return err
}

func (s *Store) CreateHookDelivery(ctx context.Context, delivery domain.HookDelivery) error {
	now := time.Now().UTC()
	if delivery.CreatedAt.IsZero() {
		delivery.CreatedAt = now
	}
	delivery.UpdatedAt = now
	if delivery.NextAttemptAt.IsZero() {
		delivery.NextAttemptAt = now
	}
	if delivery.Status == "" {
		delivery.Status = domain.HookDeliveryStatusPending
	}
	if delivery.MaxAttempts <= 0 {
		delivery.MaxAttempts = 3
	}
	_, err := s.exec(ctx, `
INSERT INTO hook_deliveries (id, hook_id, event, bucket, key, payload_json, status, attempts, max_attempts, next_attempt_at, last_status_code, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, delivery.ID, delivery.HookID, delivery.Event, delivery.Bucket, delivery.Key, delivery.PayloadJSON, delivery.Status, delivery.Attempts, delivery.MaxAttempts, delivery.NextAttemptAt.Format(time.RFC3339Nano), delivery.LastStatusCode, delivery.LastError, delivery.CreatedAt.Format(time.RFC3339Nano), delivery.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("create hook delivery: %w", err)
	}
	return nil
}

func (s *Store) GetHookDelivery(ctx context.Context, id string) (domain.HookDelivery, error) {
	row := s.queryRow(ctx, `SELECT id, hook_id, event, bucket, key, payload_json, status, attempts, max_attempts, next_attempt_at, last_status_code, last_error, created_at, updated_at FROM hook_deliveries WHERE id = ?`, id)
	delivery, err := scanHookDelivery(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.HookDelivery{}, ErrNotFound
		}
		return domain.HookDelivery{}, err
	}
	return delivery, nil
}

func (s *Store) ListHookDeliveries(ctx context.Context, limit int) ([]domain.HookDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT id, hook_id, event, bucket, key, payload_json, status, attempts, max_attempts, next_attempt_at, last_status_code, last_error, created_at, updated_at FROM hook_deliveries ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list hook deliveries: %w", err)
	}
	defer rows.Close()
	return scanHookDeliveries(rows)
}

func (s *Store) ListPendingHookDeliveries(ctx context.Context, now time.Time, limit int) ([]domain.HookDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT id, hook_id, event, bucket, key, payload_json, status, attempts, max_attempts, next_attempt_at, last_status_code, last_error, created_at, updated_at FROM hook_deliveries WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at ASC LIMIT ?`, domain.HookDeliveryStatusPending, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending hook deliveries: %w", err)
	}
	defer rows.Close()
	return scanHookDeliveries(rows)
}

func (s *Store) UpdateHookDelivery(ctx context.Context, delivery domain.HookDelivery) error {
	delivery.UpdatedAt = time.Now().UTC()
	_, err := s.exec(ctx, `
UPDATE hook_deliveries
SET status = ?, attempts = ?, next_attempt_at = ?, last_status_code = ?, last_error = ?, updated_at = ?
WHERE id = ?
`, delivery.Status, delivery.Attempts, delivery.NextAttemptAt.UTC().Format(time.RFC3339Nano), delivery.LastStatusCode, delivery.LastError, delivery.UpdatedAt.Format(time.RFC3339Nano), delivery.ID)
	if err != nil {
		return fmt.Errorf("update hook delivery: %w", err)
	}
	return nil
}

func (s *Store) UpsertBucket(ctx context.Context, b domain.Bucket) error {
	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	replicationProviderIDs, err := json.Marshal(b.ReplicationProviderIDs)
	if err != nil {
		return fmt.Errorf("encode bucket replication providers: %w", err)
	}
	_, err = s.exec(ctx, `
INSERT INTO buckets (name, replication_enabled, replication_provider_ids_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET replication_enabled=excluded.replication_enabled, replication_provider_ids_json=excluded.replication_provider_ids_json, updated_at=excluded.updated_at
`, b.Name, boolToInt(b.ReplicationEnabled), string(replicationProviderIDs), b.CreatedAt.Format(time.RFC3339Nano), b.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetBucket(ctx context.Context, name string) (domain.Bucket, error) {
	var b domain.Bucket
	var replication int
	var replicationProviderIDsJSON string
	var created, updated string
	err := s.queryRow(ctx, `SELECT name, replication_enabled, replication_provider_ids_json, created_at, updated_at FROM buckets WHERE name = ?`, name).Scan(&b.Name, &replication, &replicationProviderIDsJSON, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Bucket{}, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	b.ReplicationEnabled = replication == 1
	_ = json.Unmarshal([]byte(replicationProviderIDsJSON), &b.ReplicationProviderIDs)
	b.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	b.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return b, nil
}

func (s *Store) ListBuckets(ctx context.Context) ([]domain.Bucket, error) {
	rows, err := s.query(ctx, `SELECT name, replication_enabled, replication_provider_ids_json, created_at, updated_at FROM buckets ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Bucket
	for rows.Next() {
		var b domain.Bucket
		var replication int
		var replicationProviderIDsJSON string
		var created, updated string
		if err := rows.Scan(&b.Name, &replication, &replicationProviderIDsJSON, &created, &updated); err != nil {
			return nil, err
		}
		b.ReplicationEnabled = replication == 1
		_ = json.Unmarshal([]byte(replicationProviderIDsJSON), &b.ReplicationProviderIDs)
		b.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		b.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) PutObject(ctx context.Context, obj domain.ObjectRecord) error {
	now := time.Now().UTC()
	if obj.CreatedAt.IsZero() {
		obj.CreatedAt = now
	}
	obj.UpdatedAt = now
	_, err := s.exec(ctx, `
INSERT INTO objects (bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, replica_status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket, key) DO UPDATE SET
  provider_account_id=excluded.provider_account_id,
  remote_bucket=excluded.remote_bucket,
  remote_key=excluded.remote_key,
  size=excluded.size,
  content_type=excluded.content_type,
  etag=excluded.etag,
  checksum_sha256=excluded.checksum_sha256,
  replica_status=excluded.replica_status,
  updated_at=excluded.updated_at
`, obj.Bucket, obj.Key, obj.ProviderAccountID, obj.RemoteBucket, obj.RemoteKey, obj.Size, obj.ContentType, obj.ETag, obj.ChecksumSHA256, obj.ReplicaStatus, obj.CreatedAt.Format(time.RFC3339Nano), obj.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetObject(ctx context.Context, bucket, key string) (domain.ObjectRecord, error) {
	row := s.queryRow(ctx, `SELECT bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, replica_status, created_at, updated_at FROM objects WHERE bucket = ? AND key = ?`, bucket, key)
	obj, err := scanObject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ObjectRecord{}, ErrNotFound
	}
	return obj, err
}

func (s *Store) ListObjects(ctx context.Context, bucket, prefix string, limit int) ([]domain.ObjectRecord, error) {
	return s.ListObjectsAfter(ctx, bucket, prefix, "", limit)
}

func (s *Store) ListObjectsAfter(ctx context.Context, bucket, prefix, startAfter string, limit int) ([]domain.ObjectRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.query(ctx, `SELECT bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, replica_status, created_at, updated_at FROM objects WHERE bucket = ? AND key LIKE ? AND key > ? ORDER BY key ASC LIMIT ?`, bucket, prefix+"%", startAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ObjectRecord
	for rows.Next() {
		obj, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

func (s *Store) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := s.exec(ctx, `DELETE FROM objects WHERE bucket = ? AND key = ?`, bucket, key)
	return err
}

func (s *Store) UpsertObjectReplica(ctx context.Context, replica domain.ObjectReplica) error {
	now := time.Now().UTC()
	if replica.CreatedAt.IsZero() {
		replica.CreatedAt = now
	}
	replica.UpdatedAt = now
	_, err := s.exec(ctx, `
INSERT INTO object_replicas (bucket, key, provider_account_id, remote_bucket, remote_key, size, etag, status, error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket, key, provider_account_id) DO UPDATE SET
  remote_bucket=excluded.remote_bucket,
  remote_key=excluded.remote_key,
  size=excluded.size,
  etag=excluded.etag,
  status=excluded.status,
  error=excluded.error,
  updated_at=excluded.updated_at
`, replica.Bucket, replica.Key, replica.ProviderAccountID, replica.RemoteBucket, replica.RemoteKey, replica.Size, replica.ETag, replica.Status, replica.Error, replica.CreatedAt.Format(time.RFC3339Nano), replica.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListObjectReplicas(ctx context.Context, bucket, key string) ([]domain.ObjectReplica, error) {
	rows, err := s.query(ctx, `SELECT bucket, key, provider_account_id, remote_bucket, remote_key, size, etag, status, error, created_at, updated_at FROM object_replicas WHERE bucket = ? AND key = ? ORDER BY provider_account_id ASC`, bucket, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ObjectReplica
	for rows.Next() {
		replica, err := scanObjectReplica(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, replica)
	}
	return out, rows.Err()
}

func (s *Store) DeleteObjectReplicas(ctx context.Context, bucket, key string) error {
	_, err := s.exec(ctx, `DELETE FROM object_replicas WHERE bucket = ? AND key = ?`, bucket, key)
	return err
}

func (s *Store) ListProviderBucketUsage(ctx context.Context) ([]domain.ProviderBucketUsage, error) {
	rows, err := s.query(ctx, `SELECT provider_account_id, bucket, COUNT(*), COALESCE(SUM(size), 0) FROM objects GROUP BY provider_account_id, bucket ORDER BY provider_account_id ASC, bucket ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ProviderBucketUsage
	for rows.Next() {
		var usage domain.ProviderBucketUsage
		if err := rows.Scan(&usage.ProviderAccountID, &usage.Bucket, &usage.ObjectCount, &usage.Bytes); err != nil {
			return nil, err
		}
		out = append(out, usage)
	}
	return out, rows.Err()
}

func (s *Store) AddProviderUsage(ctx context.Context, providerID string, delta int64) error {
	_, err := s.exec(ctx, `UPDATE provider_accounts SET used_bytes = CASE WHEN used_bytes + ? < 0 THEN 0 ELSE used_bytes + ? END, updated_at = ? WHERE id = ?`, delta, delta, time.Now().UTC().Format(time.RFC3339Nano), providerID)
	return err
}

func (s *Store) CreateMultipartUpload(ctx context.Context, upload domain.MultipartUpload) error {
	now := time.Now().UTC()
	if upload.CreatedAt.IsZero() {
		upload.CreatedAt = now
	}
	upload.UpdatedAt = now
	_, err := s.exec(ctx, `INSERT INTO multipart_uploads (upload_id, bucket, key, content_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, upload.UploadID, upload.Bucket, upload.Key, upload.ContentType, upload.CreatedAt.Format(time.RFC3339Nano), upload.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetMultipartUpload(ctx context.Context, uploadID string) (domain.MultipartUpload, error) {
	var upload domain.MultipartUpload
	var created, updated string
	err := s.queryRow(ctx, `SELECT upload_id, bucket, key, content_type, created_at, updated_at FROM multipart_uploads WHERE upload_id = ?`, uploadID).Scan(&upload.UploadID, &upload.Bucket, &upload.Key, &upload.ContentType, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MultipartUpload{}, ErrNotFound
	}
	if err != nil {
		return domain.MultipartUpload{}, err
	}
	upload.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	upload.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return upload, nil
}

func (s *Store) UpsertMultipartPart(ctx context.Context, part domain.MultipartPart) error {
	now := time.Now().UTC()
	if part.CreatedAt.IsZero() {
		part.CreatedAt = now
	}
	part.UpdatedAt = now
	_, err := s.exec(ctx, `
INSERT INTO multipart_parts (upload_id, part_number, path, size, etag, checksum_sha256, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(upload_id, part_number) DO UPDATE SET
  path=excluded.path,
  size=excluded.size,
  etag=excluded.etag,
  checksum_sha256=excluded.checksum_sha256,
  updated_at=excluded.updated_at
`, part.UploadID, part.PartNumber, part.Path, part.Size, part.ETag, part.ChecksumSHA256, part.CreatedAt.Format(time.RFC3339Nano), part.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListMultipartParts(ctx context.Context, uploadID string) ([]domain.MultipartPart, error) {
	rows, err := s.query(ctx, `SELECT upload_id, part_number, path, size, etag, checksum_sha256, created_at, updated_at FROM multipart_parts WHERE upload_id = ? ORDER BY part_number ASC`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MultipartPart
	for rows.Next() {
		var part domain.MultipartPart
		var created, updated string
		if err := rows.Scan(&part.UploadID, &part.PartNumber, &part.Path, &part.Size, &part.ETag, &part.ChecksumSHA256, &created, &updated); err != nil {
			return nil, err
		}
		part.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		part.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, part)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	_, err := s.exec(ctx, `DELETE FROM multipart_uploads WHERE upload_id = ?`, uploadID)
	return err
}

var ErrNotFound = errors.New("not found")

type scanner interface{ Scan(dest ...any) error }

func scanProvider(row scanner) (domain.ProviderAccount, error) {
	var p domain.ProviderAccount
	var kind string
	var enabled int
	var settingsJSON string
	var created, updated string
	if err := row.Scan(&p.ID, &p.Name, &kind, &p.Endpoint, &p.Region, &p.Bucket, &p.AccessKey, &p.SecretEncrypted, &p.CapacityBytes, &p.UsedBytes, &p.Priority, &enabled, &settingsJSON, &created, &updated); err != nil {
		return p, err
	}
	p.Kind = domain.ProviderKind(kind)
	p.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(settingsJSON), &p.Settings)
	if p.Settings == nil {
		p.Settings = map[string]string{}
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func scanObject(row scanner) (domain.ObjectRecord, error) {
	var obj domain.ObjectRecord
	var created, updated string
	if err := row.Scan(&obj.Bucket, &obj.Key, &obj.ProviderAccountID, &obj.RemoteBucket, &obj.RemoteKey, &obj.Size, &obj.ContentType, &obj.ETag, &obj.ChecksumSHA256, &obj.ReplicaStatus, &created, &updated); err != nil {
		return obj, err
	}
	obj.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	obj.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return obj, nil
}

func scanObjectReplica(row scanner) (domain.ObjectReplica, error) {
	var replica domain.ObjectReplica
	var created, updated string
	if err := row.Scan(&replica.Bucket, &replica.Key, &replica.ProviderAccountID, &replica.RemoteBucket, &replica.RemoteKey, &replica.Size, &replica.ETag, &replica.Status, &replica.Error, &created, &updated); err != nil {
		return replica, err
	}
	replica.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	replica.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return replica, nil
}

func scanHook(row scanner) (domain.Hook, error) {
	var hook domain.Hook
	var kind string
	var eventsJSON string
	var enabled int
	var created, updated string
	if err := row.Scan(&hook.ID, &hook.Name, &kind, &hook.URL, &hook.Method, &eventsJSON, &hook.HeadersEncrypted, &enabled, &created, &updated); err != nil {
		return hook, err
	}
	hook.Kind = domain.HookKind(kind)
	hook.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(eventsJSON), &hook.Events)
	if hook.Events == nil {
		hook.Events = []string{}
	}
	hook.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	hook.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return hook, nil
}

func scanHookDelivery(row scanner) (domain.HookDelivery, error) {
	var delivery domain.HookDelivery
	var nextAttemptAt, created, updated string
	if err := row.Scan(&delivery.ID, &delivery.HookID, &delivery.Event, &delivery.Bucket, &delivery.Key, &delivery.PayloadJSON, &delivery.Status, &delivery.Attempts, &delivery.MaxAttempts, &nextAttemptAt, &delivery.LastStatusCode, &delivery.LastError, &created, &updated); err != nil {
		return delivery, err
	}
	delivery.NextAttemptAt, _ = time.Parse(time.RFC3339Nano, nextAttemptAt)
	delivery.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	delivery.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return delivery, nil
}

type rowsScanner interface {
	scanner
	Next() bool
	Err() error
}

func scanHookDeliveries(rows rowsScanner) ([]domain.HookDelivery, error) {
	var out []domain.HookDelivery
	for rows.Next() {
		delivery, err := scanHookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, delivery)
	}
	return out, rows.Err()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) CreateMigrationJob(ctx context.Context, job domain.MigrationJob) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = domain.MigrationStatusPending
	}
	_, err := s.exec(ctx, `
INSERT INTO migration_jobs (id, bucket, prefix, source_provider_id, target_provider_id, mode, status, total_objects, processed_objects, succeeded_objects, failed_objects, current_key, last_error, started_at, finished_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, job.ID, job.Bucket, job.Prefix, job.SourceProviderID, job.TargetProviderID, job.Mode, job.Status, job.TotalObjects, job.ProcessedObjects, job.SucceededObjects, job.FailedObjects, job.CurrentKey, job.LastError, formatOptionalTime(job.StartedAt), formatOptionalTime(job.FinishedAt), job.CreatedAt.Format(time.RFC3339Nano), job.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetMigrationJob(ctx context.Context, id string) (domain.MigrationJob, error) {
	row := s.queryRow(ctx, `SELECT id, bucket, prefix, source_provider_id, target_provider_id, mode, status, total_objects, processed_objects, succeeded_objects, failed_objects, current_key, last_error, started_at, finished_at, created_at, updated_at FROM migration_jobs WHERE id = ?`, id)
	job, err := scanMigrationJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MigrationJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ListMigrationJobs(ctx context.Context, limit int) ([]domain.MigrationJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.query(ctx, `SELECT id, bucket, prefix, source_provider_id, target_provider_id, mode, status, total_objects, processed_objects, succeeded_objects, failed_objects, current_key, last_error, started_at, finished_at, created_at, updated_at FROM migration_jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MigrationJob
	for rows.Next() {
		job, err := scanMigrationJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) ClaimNextMigrationJob(ctx context.Context) (domain.MigrationJob, bool, error) {
	rows, err := s.query(ctx, `SELECT id FROM migration_jobs WHERE status = ? ORDER BY created_at ASC LIMIT 5`, domain.MigrationStatusPending)
	if err != nil {
		return domain.MigrationJob{}, false, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return domain.MigrationJob{}, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return domain.MigrationJob{}, false, err
	}
	for _, id := range ids {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := s.exec(ctx, `UPDATE migration_jobs SET status = ?, started_at = ?, updated_at = ? WHERE id = ? AND status = ?`, domain.MigrationStatusRunning, now, now, id, domain.MigrationStatusPending)
		if err != nil {
			return domain.MigrationJob{}, false, err
		}
		changed, _ := res.RowsAffected()
		if changed == 1 {
			job, err := s.GetMigrationJob(ctx, id)
			return job, true, err
		}
	}
	return domain.MigrationJob{}, false, nil
}

func (s *Store) UpdateMigrationJob(ctx context.Context, job domain.MigrationJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.exec(ctx, `
UPDATE migration_jobs SET
  status = ?, total_objects = ?, processed_objects = ?, succeeded_objects = ?, failed_objects = ?, current_key = ?, last_error = ?, started_at = ?, finished_at = ?, updated_at = ?
WHERE id = ?
`, job.Status, job.TotalObjects, job.ProcessedObjects, job.SucceededObjects, job.FailedObjects, job.CurrentKey, job.LastError, formatOptionalTime(job.StartedAt), formatOptionalTime(job.FinishedAt), job.UpdatedAt.Format(time.RFC3339Nano), job.ID)
	return err
}

func scanMigrationJob(row scanner) (domain.MigrationJob, error) {
	var job domain.MigrationJob
	var started, finished, created, updated string
	if err := row.Scan(&job.ID, &job.Bucket, &job.Prefix, &job.SourceProviderID, &job.TargetProviderID, &job.Mode, &job.Status, &job.TotalObjects, &job.ProcessedObjects, &job.SucceededObjects, &job.FailedObjects, &job.CurrentKey, &job.LastError, &started, &finished, &created, &updated); err != nil {
		return job, err
	}
	job.StartedAt = parseOptionalTime(started)
	job.FinishedAt = parseOptionalTime(finished)
	job.CreatedAt = parseOptionalTime(created)
	job.UpdatedAt = parseOptionalTime(updated)
	return job, nil
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseOptionalTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
