package domain

import "time"

type ProviderKind string

const (
	ProviderKindLocal      ProviderKind = "local"
	ProviderKindS3Compat   ProviderKind = "s3-compatible"
	ProviderKindAzureBlob  ProviderKind = "azure-blob"
	ProviderKindCloudinary ProviderKind = "cloudinary"
	ProviderKindVercelBlob ProviderKind = "vercel-blob"
)

type ProviderAccount struct {
	ID                   string
	Name                 string
	Kind                 ProviderKind
	Endpoint             string
	Region               string
	Bucket               string
	AccessKey            string
	SecretEncrypted      string
	SecretKey            string
	CapacityBytes        int64
	UsedBytes            int64
	RemoteCapacityBytes  int64
	RemoteUsedBytes      int64
	ReservedBytes        int64
	MonthlyUploadedBytes int64
	MonthlyPeriod        string
	Priority             int
	Enabled              bool
	AvailabilityStatus   string
	AvailabilityMessage  string
	UnavailableUntil     time.Time
	QuotaSource          string
	QuotaCheckedAt       time.Time
	Settings             map[string]string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Bucket struct {
	Name                   string          `json:"name"`
	ReplicationEnabled     bool            `json:"replication_enabled"`
	ReplicationProviderIDs []string        `json:"replication_provider_ids"`
	VersioningEnabled      bool            `json:"versioning_enabled"`
	TrashEnabled           bool            `json:"trash_enabled"`
	TrashRetentionDays     int             `json:"trash_retention_days"`
	ObjectLockEnabled      bool            `json:"object_lock_enabled"`
	DefaultRetentionMode   string          `json:"default_retention_mode"`
	DefaultRetentionDays   int             `json:"default_retention_days"`
	LifecycleRules         []LifecycleRule `json:"lifecycle_rules,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

type LifecycleRule struct {
	ID                  string `json:"id"`
	Prefix              string `json:"prefix,omitempty"`
	ExpireAfterDays     int    `json:"expire_after_days,omitempty"`
	PurgeTrashAfterDays int    `json:"purge_trash_after_days,omitempty"`
	Enabled             bool   `json:"enabled"`
}

type ObjectRecord struct {
	Bucket            string
	Key               string
	ProviderAccountID string
	RemoteBucket      string
	RemoteKey         string
	Size              int64
	ContentType       string
	ETag              string
	ChecksumSHA256    string
	Metadata          map[string]string
	Tags              map[string]string
	VersionID         string
	IsDeleteMarker    bool
	RetentionMode     string
	RetainUntil       time.Time
	LegalHold         bool
	ReplicaStatus     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type TrashRecord struct {
	ID         string       `json:"id"`
	Object     ObjectRecord `json:"object"`
	DeletedAt  time.Time    `json:"deleted_at"`
	PurgeAfter time.Time    `json:"purge_after"`
}

type PutObjectInput struct {
	Bucket         string
	Key            string
	RemoteKey      string
	Size           int64
	ContentType    string
	Metadata       map[string]string
	Tags           map[string]string
	RetentionMode  string
	RetainUntil    time.Time
	LegalHold      bool
	ChecksumSHA256 string
	// SkipWASMPipelines prevents derived objects from recursively triggering the
	// same processing graph. It is an internal orchestration flag and is never
	// persisted or exposed through the S3 surface.
	SkipWASMPipelines bool
}

func (p PutObjectInput) StorageKey() string {
	if p.RemoteKey != "" {
		return p.RemoteKey
	}
	return p.Key
}

type StoredObject struct {
	ProviderAccountID string
	RemoteBucket      string
	RemoteKey         string
	Size              int64
	ContentType       string
	ETag              string
	ChecksumSHA256    string
}

type ProviderObject struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	ContentType  string    `json:"content_type,omitempty"`
	LastModified time.Time `json:"last_modified"`
}

type ProviderObjectPage struct {
	Objects               []ProviderObject `json:"objects"`
	NextContinuationToken string           `json:"next_continuation_token,omitempty"`
}

type ProviderBucket struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type AccessCredential struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	AccessKey       string    `json:"access_key"`
	SecretEncrypted string    `json:"-"`
	SecretKey       string    `json:"-"`
	Role            string    `json:"role"`
	Permissions     []string  `json:"permissions"`
	BucketPatterns  []string  `json:"bucket_patterns"`
	PrefixPatterns  []string  `json:"prefix_patterns"`
	Enabled         bool      `json:"enabled"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	LastUsedAt      time.Time `json:"last_used_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const (
	AccessRoleAdmin     = "admin"
	AccessRoleReadWrite = "read-write"
	AccessRoleReadOnly  = "read-only"
)

type InventoryJob struct {
	ID                string    `json:"id"`
	ProviderAccountID string    `json:"provider_account_id"`
	Bucket            string    `json:"bucket"`
	RemoteBucket      string    `json:"remote_bucket"`
	Prefix            string    `json:"prefix"`
	Mode              string    `json:"mode"`
	Status            string    `json:"status"`
	DiscoveredObjects int       `json:"discovered_objects"`
	ImportedObjects   int       `json:"imported_objects"`
	MissingObjects    int       `json:"missing_objects"`
	LastError         string    `json:"last_error"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	FinishedAt        time.Time `json:"finished_at,omitempty"`
}

type RepairJob struct {
	ID              string    `json:"id"`
	Bucket          string    `json:"bucket"`
	Prefix          string    `json:"prefix"`
	Status          string    `json:"status"`
	CheckedObjects  int       `json:"checked_objects"`
	RepairedObjects int       `json:"repaired_objects"`
	FailedObjects   int       `json:"failed_objects"`
	CurrentKey      string    `json:"current_key"`
	LastError       string    `json:"last_error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

const (
	RepairStatusPending   = "pending"
	RepairStatusRunning   = "running"
	RepairStatusCompleted = "completed"
	RepairStatusFailed    = "failed"
)

const (
	InventoryModeImport      = "import"
	InventoryModeReconcile   = "reconcile"
	InventoryStatusPending   = "pending"
	InventoryStatusRunning   = "running"
	InventoryStatusCompleted = "completed"
	InventoryStatusFailed    = "failed"
)

type ObjectReplica struct {
	Bucket            string
	Key               string
	ProviderAccountID string
	RemoteBucket      string
	RemoteKey         string
	Size              int64
	ETag              string
	ChecksumSHA256    string
	Status            string
	Error             string
	Attempts          int
	MaxAttempts       int
	NextAttemptAt     time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProviderReservation struct {
	ID                string
	ProviderAccountID string
	Bytes             int64
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

type ProviderQuota struct {
	ProviderAccountID    string    `json:"provider_account_id"`
	CapacityBytes        int64     `json:"capacity_bytes"`
	UsedBytes            int64     `json:"used_bytes"`
	ReservedBytes        int64     `json:"reserved_bytes"`
	AvailableBytes       int64     `json:"available_bytes"`
	MonthlyLimitBytes    int64     `json:"monthly_limit_bytes"`
	MonthlyUploadedBytes int64     `json:"monthly_uploaded_bytes"`
	Period               string    `json:"period"`
	Source               string    `json:"source"`
	CheckedAt            time.Time `json:"checked_at,omitempty"`
	Reliable             bool      `json:"reliable"`
}

type ProviderCapabilities struct {
	ListObjects     bool     `json:"list_objects"`
	DiscoverBuckets bool     `json:"discover_buckets"`
	RemoteQuota     bool     `json:"remote_quota"`
	Checksums       bool     `json:"checksums"`
	Limitations     []string `json:"limitations,omitempty"`
}

type ProviderValidation struct {
	ProviderAccountID string               `json:"provider_account_id"`
	Health            ProviderHealth       `json:"health"`
	Capabilities      ProviderCapabilities `json:"capabilities"`
}

const (
	MigrationModeCopy = "copy"
	MigrationModeMove = "move"

	MigrationStatusPending   = "pending"
	MigrationStatusRunning   = "running"
	MigrationStatusCompleted = "completed"
	MigrationStatusFailed    = "failed"
)

type MigrationJob struct {
	ID               string    `json:"id"`
	Bucket           string    `json:"bucket"`
	Prefix           string    `json:"prefix"`
	SourceProviderID string    `json:"source_provider_id"`
	TargetProviderID string    `json:"target_provider_id"`
	Mode             string    `json:"mode"`
	Status           string    `json:"status"`
	TotalObjects     int       `json:"total_objects"`
	ProcessedObjects int       `json:"processed_objects"`
	SucceededObjects int       `json:"succeeded_objects"`
	FailedObjects    int       `json:"failed_objects"`
	CurrentKey       string    `json:"current_key"`
	LastError        string    `json:"last_error"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
