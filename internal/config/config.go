package config

import (
	"errors"
	"fmt"
	"net/url"
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
	Addr                  string `yaml:"addr"`
	DataDir               string `yaml:"data_dir"`
	MultipartStagingDir   string `yaml:"multipart_staging_dir"`
	DBPath                string `yaml:"db_path"`
	MasterKey             string `yaml:"master_key"`
	PublicBaseURL         string `yaml:"public_base_url"`
	MaxUploadBytes        int64  `yaml:"max_upload_bytes"`
	MaxMultipartPartBytes int64  `yaml:"max_multipart_part_bytes"`
	MaxAdminBodyBytes     int64  `yaml:"max_admin_body_bytes"`
}

type StoreConfig struct {
	Kind     string              `yaml:"kind"`
	SQLite   SQLiteStoreConfig   `yaml:"sqlite"`
	Postgres PostgresStoreConfig `yaml:"postgres"`
}

type SQLiteStoreConfig struct {
	Path         string `yaml:"path"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
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
	Enabled  bool       `yaml:"enabled"`
	Username string     `yaml:"username"`
	Password string     `yaml:"password"`
	OIDC     OIDCConfig `yaml:"oidc"`
}

type OIDCConfig struct {
	Enabled       bool     `yaml:"enabled"`
	IssuerURL     string   `yaml:"issuer_url"`
	ClientID      string   `yaml:"client_id"`
	ClientSecret  string   `yaml:"client_secret"`
	RedirectURL   string   `yaml:"redirect_url"`
	Scopes        []string `yaml:"scopes"`
	AllowedGroups []string `yaml:"allowed_groups"`
	SessionHours  int      `yaml:"session_hours"`
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
	Name                   string                `yaml:"name"`
	ReplicationEnabled     bool                  `yaml:"replication_enabled"`
	ReplicationProviderIDs []string              `yaml:"replication_provider_ids"`
	VersioningEnabled      *bool                 `yaml:"versioning_enabled,omitempty"`
	TrashEnabled           *bool                 `yaml:"trash_enabled,omitempty"`
	TrashRetentionDays     *int                  `yaml:"trash_retention_days,omitempty"`
	ObjectLockEnabled      *bool                 `yaml:"object_lock_enabled,omitempty"`
	DefaultRetentionMode   *string               `yaml:"default_retention_mode,omitempty"`
	DefaultRetentionDays   *int                  `yaml:"default_retention_days,omitempty"`
	LifecycleRules         []LifecycleRuleConfig `yaml:"lifecycle_rules,omitempty"`
}

type LifecycleRuleConfig struct {
	ID                  string `yaml:"id"`
	Prefix              string `yaml:"prefix,omitempty"`
	ExpireAfterDays     int    `yaml:"expire_after_days,omitempty"`
	PurgeTrashAfterDays int    `yaml:"purge_trash_after_days,omitempty"`
	Enabled             bool   `yaml:"enabled"`
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
		Server: ServerConfig{
			Addr:                  ":8080",
			DataDir:               "/data",
			DBPath:                "/data/switcher.db",
			MaxUploadBytes:        5 << 30,
			MaxMultipartPartBytes: 512 << 20,
			MaxAdminBodyBytes:     1 << 20,
		},
		Store:        StoreConfig{Kind: "sqlite", SQLite: SQLiteStoreConfig{Path: "/data/switcher.db", MaxOpenConns: 10, MaxIdleConns: 10}, Postgres: PostgresStoreConfig{MaxOpenConns: 25, MaxIdleConns: 10}},
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
	if c.Server.MaxUploadBytes <= 0 {
		return errors.New("server.max_upload_bytes must be greater than zero")
	}
	if c.Server.MaxMultipartPartBytes <= 0 || c.Server.MaxMultipartPartBytes > c.Server.MaxUploadBytes {
		return errors.New("server.max_multipart_part_bytes must be greater than zero and no larger than max_upload_bytes")
	}
	if c.Server.MaxAdminBodyBytes <= 0 {
		return errors.New("server.max_admin_body_bytes must be greater than zero")
	}
	if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
		return errors.New("s3.access_key and s3.secret_key are required")
	}
	if c.Admin.Enabled && (c.Admin.Username == "" || c.Admin.Password == "") {
		if !c.Admin.OIDC.Enabled {
			return errors.New("admin username/password or OIDC are required when admin is enabled")
		}
	}
	if c.Admin.OIDC.Enabled {
		if c.Admin.OIDC.IssuerURL == "" || c.Admin.OIDC.ClientID == "" || c.Admin.OIDC.ClientSecret == "" || c.Admin.OIDC.RedirectURL == "" {
			return errors.New("admin OIDC issuer_url, client_id, client_secret and redirect_url are required")
		}
		for name, raw := range map[string]string{"issuer_url": c.Admin.OIDC.IssuerURL, "redirect_url": c.Admin.OIDC.RedirectURL} {
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return fmt.Errorf("admin OIDC %s must be an absolute URL", name)
			}
		}
	}
	for _, p := range c.Providers {
		if p.ID == "" || p.Kind == "" || p.Bucket == "" {
			return fmt.Errorf("provider %q must define id, kind and bucket", p.Name)
		}
	}
	for _, bucket := range c.Buckets {
		if bucket.Name == "" {
			return errors.New("every bucket must define a name")
		}
		if bucket.TrashRetentionDays != nil && *bucket.TrashRetentionDays < 0 {
			return fmt.Errorf("bucket %q trash_retention_days cannot be negative", bucket.Name)
		}
		if bucket.DefaultRetentionDays != nil && *bucket.DefaultRetentionDays < 0 {
			return fmt.Errorf("bucket %q default_retention_days cannot be negative", bucket.Name)
		}
		if bucket.DefaultRetentionMode != nil && *bucket.DefaultRetentionMode != "" && !strings.EqualFold(*bucket.DefaultRetentionMode, "governance") && !strings.EqualFold(*bucket.DefaultRetentionMode, "compliance") {
			return fmt.Errorf("bucket %q default_retention_mode must be governance or compliance", bucket.Name)
		}
		for _, rule := range bucket.LifecycleRules {
			if rule.ID == "" || rule.ExpireAfterDays < 0 || rule.PurgeTrashAfterDays < 0 {
				return fmt.Errorf("bucket %q lifecycle rules require an id and non-negative day values", bucket.Name)
			}
		}
	}
	return nil
}

func (c *Config) Normalize() {
	if c.Server.DataDir == "" {
		c.Server.DataDir = "/data"
	}
	if c.Server.MultipartStagingDir == "" {
		c.Server.MultipartStagingDir = strings.TrimRight(c.Server.DataDir, "/") + "/multipart"
	}
	if c.Server.MaxUploadBytes == 0 {
		c.Server.MaxUploadBytes = 5 << 30
	}
	if c.Server.MaxMultipartPartBytes == 0 {
		c.Server.MaxMultipartPartBytes = 512 << 20
	}
	if c.Server.MaxAdminBodyBytes == 0 {
		c.Server.MaxAdminBodyBytes = 1 << 20
	}
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
	if c.Store.SQLite.MaxOpenConns == 0 {
		c.Store.SQLite.MaxOpenConns = 10
	}
	if c.Store.SQLite.MaxIdleConns == 0 {
		c.Store.SQLite.MaxIdleConns = 10
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
	if len(c.Admin.OIDC.Scopes) == 0 {
		c.Admin.OIDC.Scopes = []string{"openid", "profile", "email"}
	}
	if c.Admin.OIDC.SessionHours <= 0 {
		c.Admin.OIDC.SessionHours = 8
	}
}

func applyEnv(c *Config) {
	setString(&c.Server.Addr, "ADDR")
	setString(&c.Server.DataDir, "DATA_DIR")
	setString(&c.Server.MultipartStagingDir, "MULTIPART_STAGING_DIR")
	if value := strings.TrimSpace(os.Getenv("DB_PATH")); value != "" {
		c.Server.DBPath = value
		c.Store.SQLite.Path = value
	}
	setString(&c.Server.MasterKey, "MASTER_KEY")
	setString(&c.Server.PublicBaseURL, "PUBLIC_BASE_URL")
	setInt64(&c.Server.MaxUploadBytes, "MAX_UPLOAD_BYTES")
	setInt64(&c.Server.MaxMultipartPartBytes, "MAX_MULTIPART_PART_BYTES")
	setInt64(&c.Server.MaxAdminBodyBytes, "MAX_ADMIN_BODY_BYTES")
	setString(&c.Store.Kind, "STORE_KIND")
	setString(&c.Store.SQLite.Path, "SQLITE_PATH")
	setInt(&c.Store.SQLite.MaxOpenConns, "SQLITE_MAX_OPEN_CONNS")
	setInt(&c.Store.SQLite.MaxIdleConns, "SQLITE_MAX_IDLE_CONNS")
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
	setString(&c.Admin.OIDC.IssuerURL, "ADMIN_OIDC_ISSUER_URL")
	setString(&c.Admin.OIDC.ClientID, "ADMIN_OIDC_CLIENT_ID")
	setString(&c.Admin.OIDC.ClientSecret, "ADMIN_OIDC_CLIENT_SECRET")
	setString(&c.Admin.OIDC.RedirectURL, "ADMIN_OIDC_REDIRECT_URL")
	setInt(&c.Admin.OIDC.SessionHours, "ADMIN_OIDC_SESSION_HOURS")
	if value := strings.TrimSpace(os.Getenv("ADMIN_OIDC_ENABLED")); value != "" {
		c.Admin.OIDC.Enabled, _ = strconv.ParseBool(value)
	}
	if value := strings.TrimSpace(os.Getenv("ADMIN_OIDC_ALLOWED_GROUPS")); value != "" {
		c.Admin.OIDC.AllowedGroups = splitCSV(value)
	}
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

func setInt64(target *int64, env string) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			*target = parsed
		}
	}
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
