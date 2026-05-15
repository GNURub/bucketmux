package config

import (
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MASTER_KEY", "test-master-key")
	t.Setenv("S3_ACCESS_KEY", "ak")
	t.Setenv("S3_SECRET_KEY", "sk")
}

func TestLoadDefaultsToSQLiteStore(t *testing.T) {
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

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Store.Kind != "postgres" || cfg.Store.Postgres.DSN == "" || cfg.Store.Postgres.MaxOpenConns != 7 || cfg.Store.Postgres.MaxIdleConns != 3 {
		t.Fatalf("postgres config = %+v", cfg.Store)
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
