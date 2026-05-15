package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Store        StoreConfig        `yaml:"store"`
	Coordination CoordinationConfig `yaml:"coordination"`
	S3           S3Config           `yaml:"s3"`
	Admin        AdminConfig        `yaml:"admin"`
	Providers    []ProviderConfig   `yaml:"providers"`
	Buckets      []BucketConfig     `yaml:"buckets"`
}

type ServerConfig struct {
	Addr          string `yaml:"addr"`
	DataDir       string `yaml:"data_dir"`
	DBPath        string `yaml:"db_path"`
	MasterKey     string `yaml:"master_key"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type StoreConfig struct {
	Kind     string              `yaml:"kind"`
	SQLite   SQLiteStoreConfig   `yaml:"sqlite"`
	Postgres PostgresStoreConfig `yaml:"postgres"`
}

type SQLiteStoreConfig struct {
	Path string `yaml:"path"`
}

type PostgresStoreConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type CoordinationConfig struct {
	Kind  string      `yaml:"kind"`
	Redis RedisConfig `yaml:"redis"`
}

type RedisConfig struct {
	Addr            string `yaml:"addr"`
	Password        string `yaml:"password"`
	DB              int    `yaml:"db"`
	KeyPrefix       string `yaml:"key_prefix"`
	LeaseTTLSeconds int    `yaml:"lease_ttl_seconds"`
}

type S3Config struct {
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Region    string `yaml:"region"`
}

type AdminConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type ProviderConfig struct {
	ID            string            `yaml:"id"`
	Name          string            `yaml:"name"`
	Kind          string            `yaml:"kind"`
	Endpoint      string            `yaml:"endpoint"`
	Region        string            `yaml:"region"`
	Bucket        string            `yaml:"bucket"`
	AccessKey     string            `yaml:"access_key"`
	SecretKey     string            `yaml:"secret_key"`
	CapacityBytes int64             `yaml:"capacity_bytes"`
	Priority      int               `yaml:"priority"`
	Enabled       *bool             `yaml:"enabled"`
	Settings      map[string]string `yaml:"settings"`
}

type BucketConfig struct {
	Name                   string   `yaml:"name"`
	ReplicationEnabled     bool     `yaml:"replication_enabled"`
	ReplicationProviderIDs []string `yaml:"replication_provider_ids"`
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return cfg, fmt.Errorf("read config: %w", err)
			}
		} else if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(&cfg)
	cfg.Normalize()
	return cfg, cfg.Validate()
}

func Default() Config {
	return Config{
		Server:       ServerConfig{Addr: ":8080", DataDir: "/data", DBPath: "/data/switcher.db"},
		Store:        StoreConfig{Kind: "sqlite", SQLite: SQLiteStoreConfig{Path: "/data/switcher.db"}, Postgres: PostgresStoreConfig{MaxOpenConns: 25, MaxIdleConns: 10}},
		Coordination: CoordinationConfig{Kind: "database", Redis: RedisConfig{Addr: "127.0.0.1:6379", KeyPrefix: "bucketmux", LeaseTTLSeconds: 5}},
		S3:           S3Config{Region: "auto"},
		Admin:        AdminConfig{Enabled: false, Username: "admin"},
		Buckets:      []BucketConfig{{Name: "images"}},
	}
}

func (c Config) Validate() error {
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if c.Server.DBPath == "" {
		if c.Store.Kind == "" || c.Store.Kind == "sqlite" {
			return errors.New("server.db_path or store.sqlite.path is required")
		}
	}
	if c.Store.Kind == "" {
		c.Store.Kind = "sqlite"
	}
	switch c.Store.Kind {
	case "sqlite":
		if c.Store.SQLite.Path == "" && c.Server.DBPath == "" {
			return errors.New("store.sqlite.path or server.db_path is required")
		}
	case "postgres":
		if strings.TrimSpace(c.Store.Postgres.DSN) == "" {
			return errors.New("store.postgres.dsn or POSTGRES_DSN is required when store.kind=postgres")
		}
	default:
		return fmt.Errorf("unknown store.kind %q", c.Store.Kind)
	}
	switch c.Coordination.Kind {
	case "", "database":
	case "redis":
		if strings.TrimSpace(c.Coordination.Redis.Addr) == "" {
			return errors.New("coordination.redis.addr is required when coordination.kind=redis")
		}
	default:
		return fmt.Errorf("unknown coordination.kind %q", c.Coordination.Kind)
	}
	if c.Server.MasterKey == "" {
		return errors.New("server.master_key or MASTER_KEY is required")
	}
	if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
		return errors.New("s3.access_key and s3.secret_key are required")
	}
	if c.Admin.Enabled && (c.Admin.Username == "" || c.Admin.Password == "") {
		return errors.New("admin username/password are required when admin is enabled")
	}
	for _, p := range c.Providers {
		if p.ID == "" || p.Kind == "" || p.Bucket == "" {
			return fmt.Errorf("provider %q must define id, kind and bucket", p.Name)
		}
	}
	return nil
}

func (c *Config) Normalize() {
	c.Store.Kind = strings.TrimSpace(c.Store.Kind)
	if c.Store.Kind == "" {
		c.Store.Kind = "sqlite"
	}
	if c.Store.Postgres.MaxOpenConns == 0 {
		c.Store.Postgres.MaxOpenConns = 25
	}
	if c.Store.Postgres.MaxIdleConns == 0 {
		c.Store.Postgres.MaxIdleConns = 10
	}
	if c.Store.SQLite.Path == "" {
		c.Store.SQLite.Path = c.Server.DBPath
	}
	if c.Server.DBPath == "" {
		c.Server.DBPath = c.Store.SQLite.Path
	}
	c.Coordination.Kind = strings.TrimSpace(c.Coordination.Kind)
	if c.Coordination.Kind == "" {
		c.Coordination.Kind = "database"
	}
	if c.Coordination.Redis.Addr == "" {
		c.Coordination.Redis.Addr = "127.0.0.1:6379"
	}
	if c.Coordination.Redis.KeyPrefix == "" {
		c.Coordination.Redis.KeyPrefix = "bucketmux"
	}
	if c.Coordination.Redis.LeaseTTLSeconds == 0 {
		c.Coordination.Redis.LeaseTTLSeconds = 5
	}
}

func applyEnv(c *Config) {
	setString(&c.Server.Addr, "ADDR")
	setString(&c.Server.DataDir, "DATA_DIR")
	if value := strings.TrimSpace(os.Getenv("DB_PATH")); value != "" {
		c.Server.DBPath = value
		c.Store.SQLite.Path = value
	}
	setString(&c.Server.MasterKey, "MASTER_KEY")
	setString(&c.Server.PublicBaseURL, "PUBLIC_BASE_URL")
	setString(&c.Store.Kind, "STORE_KIND")
	setString(&c.Store.SQLite.Path, "SQLITE_PATH")
	setString(&c.Store.Postgres.DSN, "POSTGRES_DSN")
	setInt(&c.Store.Postgres.MaxOpenConns, "POSTGRES_MAX_OPEN_CONNS")
	setInt(&c.Store.Postgres.MaxIdleConns, "POSTGRES_MAX_IDLE_CONNS")
	setString(&c.Coordination.Kind, "COORDINATION_KIND")
	setString(&c.Coordination.Redis.Addr, "REDIS_ADDR")
	setString(&c.Coordination.Redis.Password, "REDIS_PASSWORD")
	setInt(&c.Coordination.Redis.DB, "REDIS_DB")
	setString(&c.Coordination.Redis.KeyPrefix, "REDIS_KEY_PREFIX")
	setInt(&c.Coordination.Redis.LeaseTTLSeconds, "REDIS_LEASE_TTL_SECONDS")
	setString(&c.S3.AccessKey, "S3_ACCESS_KEY")
	setString(&c.S3.SecretKey, "S3_SECRET_KEY")
	setString(&c.S3.Region, "S3_REGION")
	setString(&c.Admin.Username, "ADMIN_USER")
	setString(&c.Admin.Password, "ADMIN_PASSWORD")
	if v := strings.TrimSpace(os.Getenv("ADMIN_ENABLED")); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err == nil {
			c.Admin.Enabled = parsed
		}
	}
}

func setString(target *string, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		*target = value
	}
}

func setInt(target *int, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil {
			*target = parsed
		}
	}
}
