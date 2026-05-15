package domain

import "time"

type ProviderKind string

const (
	ProviderKindLocal      ProviderKind = "local"
	ProviderKindS3Compat   ProviderKind = "s3-compatible"
	ProviderKindCloudinary ProviderKind = "cloudinary"
	ProviderKindVercelBlob ProviderKind = "vercel-blob"
)

type ProviderAccount struct {
	ID              string
	Name            string
	Kind            ProviderKind
	Endpoint        string
	Region          string
	Bucket          string
	AccessKey       string
	SecretEncrypted string
	SecretKey       string
	CapacityBytes   int64
	UsedBytes       int64
	Priority        int
	Enabled         bool
	Settings        map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Bucket struct {
	Name                   string    `json:"name"`
	ReplicationEnabled     bool      `json:"replication_enabled"`
	ReplicationProviderIDs []string  `json:"replication_provider_ids"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
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
	ReplicaStatus     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type PutObjectInput struct {
	Bucket      string
	Key         string
	Size        int64
	ContentType string
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

type ObjectReplica struct {
	Bucket            string
	Key               string
	ProviderAccountID string
	RemoteBucket      string
	RemoteKey         string
	Size              int64
	ETag              string
	Status            string
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
