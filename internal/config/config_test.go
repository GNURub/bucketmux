package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MASTER_KEY", "test-master-key")
	t.Setenv("S3_ACCESS_KEY", "ak")
	t.Setenv("S3_SECRET_KEY", "sk")
}

func TestBucketProtectionConfiguration(t *testing.T) {
	setRequiredEnv(t)
	cfg := Default()
	if err := yaml.Unmarshal([]byte(`
buckets:
  - name: images
    versioning_enabled: true
    trash_enabled: true
    trash_retention_days: 30
    object_lock_enabled: true
    default_retention_mode: governance
    default_retention_days: 7
    lifecycle_rules:
      - id: temporary
        prefix: tmp/
        expire_after_days: 2
        purge_trash_after_days: 5
        enabled: true
`), &cfg); err != nil {
		t.Fatal(err)
	}
	applyEnv(&cfg)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bucket := cfg.Buckets[0]
	if bucket.VersioningEnabled == nil || !*bucket.VersioningEnabled || bucket.TrashRetentionDays == nil || *bucket.TrashRetentionDays != 30 || len(bucket.LifecycleRules) != 1 || bucket.LifecycleRules[0].PurgeTrashAfterDays != 5 {
		t.Fatalf("bucket protection=%+v", bucket)
	}
}

func TestLoadDefaultsToSQLiteStoreBackedByTurso(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Store.Kind != "sqlite" {
		t.Fatalf("Store.Kind = %q, want sqlite", cfg.Store.Kind)
	}
	if cfg.Store.SQLite.Path != cfg.Server.DBPath {
		t.Fatalf("Store.SQLite.Path = %q, Server.DBPath = %q", cfg.Store.SQLite.Path, cfg.Server.DBPath)
	}
	if cfg.Store.SQLite.MaxOpenConns != 4 || cfg.Store.SQLite.MaxIdleConns != 4 {
		t.Fatalf("Store.SQLite pool = %+v, want 4 open and 4 idle connections", cfg.Store.SQLite)
	}
}

func TestLoadSQLitePoolFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SQLITE_MAX_OPEN_CONNS", "6")
	t.Setenv("SQLITE_MAX_IDLE_CONNS", "4")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Store.SQLite.MaxOpenConns != 6 || cfg.Store.SQLite.MaxIdleConns != 4 {
		t.Fatalf("Store.SQLite pool = %+v, want 6 open and 4 idle connections", cfg.Store.SQLite)
	}
}

func TestLoadSQLiteConfigurationUsesPublicSQLiteKind(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("STORE_KIND", "sqlite")
	t.Setenv("SQLITE_PATH", "/tmp/legacy-bucketmux.db")
	t.Setenv("SQLITE_MAX_OPEN_CONNS", "3")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Kind != "sqlite" || cfg.Store.SQLite.Path != "/tmp/legacy-bucketmux.db" || cfg.Store.SQLite.MaxOpenConns != 3 {
		t.Fatalf("sqlite config was not preserved: %+v", cfg.Store)
	}
}

func TestLoadProductionLimitsAndMultipartStagingFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DATA_DIR", "/srv/bucketmux")
	t.Setenv("MULTIPART_STAGING_DIR", "/shared/multipart")
	t.Setenv("MAX_UPLOAD_BYTES", "1048576")
	t.Setenv("MAX_MULTIPART_PART_BYTES", "524288")
	t.Setenv("MAX_ADMIN_BODY_BYTES", "65536")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.MultipartStagingDir != "/shared/multipart" || cfg.Server.MaxUploadBytes != 1048576 || cfg.Server.MaxMultipartPartBytes != 524288 || cfg.Server.MaxAdminBodyBytes != 65536 {
		t.Fatalf("server production limits = %+v", cfg.Server)
	}
}

func TestLoadPostgresRequiresDSN(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("STORE_KIND", "postgres")

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "store.postgres.dsn") {
		t.Fatalf("Load() error = %v, want missing postgres dsn", err)
	}
}

func TestLoadPostgresFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("STORE_KIND", "postgres")
	t.Setenv("POSTGRES_DSN", "postgres://bucketmux:bucketmux@localhost:5432/bucketmux?sslmode=disable")
	t.Setenv("POSTGRES_MAX_OPEN_CONNS", "7")
	t.Setenv("POSTGRES_MAX_IDLE_CONNS", "3")
	t.Setenv("VECTOR_SEARCH_BACKEND", "pgvector")
	t.Setenv("VECTOR_SEARCH_EF_SEARCH", "128")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Store.Kind != "postgres" || cfg.Store.Postgres.DSN == "" || cfg.Store.Postgres.MaxOpenConns != 7 || cfg.Store.Postgres.MaxIdleConns != 3 || cfg.Store.VectorSearch.Backend != "pgvector" || cfg.Store.VectorSearch.EFSearch != 128 {
		t.Fatalf("postgres config = %+v", cfg.Store)
	}
}

func TestPgvectorBackendRequiresPostgres(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("VECTOR_SEARCH_BACKEND", "pgvector")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "requires store.kind=postgres") {
		t.Fatalf("Load() error = %v, want pgvector/postgres validation", err)
	}
}

func TestDBPathEnvOverridesLegacyAndSQLitePath(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DB_PATH", "/tmp/bucketmux-env.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.DBPath != "/tmp/bucketmux-env.db" || cfg.Store.SQLite.Path != "/tmp/bucketmux-env.db" {
		t.Fatalf("Server.DBPath=%q Store.SQLite.Path=%q", cfg.Server.DBPath, cfg.Store.SQLite.Path)
	}
}

func TestCoordinationRedisConfigFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("COORDINATION_KIND", "redis")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_DB", "2")
	t.Setenv("REDIS_KEY_PREFIX", "bucketmux-test")
	t.Setenv("REDIS_LEASE_TTL_SECONDS", "9")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Coordination.Kind != "redis" || cfg.Coordination.Redis.Addr != "redis:6379" || cfg.Coordination.Redis.DB != 2 || cfg.Coordination.Redis.KeyPrefix != "bucketmux-test" || cfg.Coordination.Redis.LeaseTTLSeconds != 9 {
		t.Fatalf("coordination config = %+v", cfg.Coordination)
	}
}
