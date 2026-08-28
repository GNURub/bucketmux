package domain

import "time"

const WASMPluginABIV1 = "bucketmux.wasm.v1"

const (
	WASMPluginEventObjectCreated = "object.created"

	WASMPluginStatusPending    = "pending"
	WASMPluginStatusRunning    = "running"
	WASMPluginStatusSucceeded  = "succeeded"
	WASMPluginStatusFailed     = "failed"
	WASMPluginStatusSuperseded = "superseded"
)

// WASMPlugin is a WASI command module plus the routing and resource policy
// required to invoke it safely. ModuleBase64 is deliberately omitted from JSON
// responses so listing plugins never downloads executable payloads.
type WASMPlugin struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	ABIVersion       string            `json:"abi_version"`
	ModuleBase64     string            `json:"-"`
	ModuleSHA256     string            `json:"module_sha256"`
	Events           []string          `json:"events"`
	BucketPattern    string            `json:"bucket_pattern"`
	KeyPrefix        string            `json:"key_prefix,omitempty"`
	KeySuffix        string            `json:"key_suffix,omitempty"`
	ContentTypes     []string          `json:"content_types,omitempty"`
	Config           map[string]string `json:"config,omitempty"`
	Enabled          bool              `json:"enabled"`
	TimeoutMillis    int               `json:"timeout_millis"`
	MemoryLimitBytes int64             `json:"memory_limit_bytes"`
	MaxInputBytes    int64             `json:"max_input_bytes"`
	MaxOutputBytes   int64             `json:"max_output_bytes"`
	MaxAttempts      int               `json:"max_attempts"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type WASMPluginJob struct {
	ID              string    `json:"id"`
	PluginID        string    `json:"plugin_id"`
	Event           string    `json:"event"`
	Bucket          string    `json:"bucket"`
	Key             string    `json:"key"`
	SourceChecksum  string    `json:"source_checksum"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
	DedupeKey       string    `json:"dedupe_key"`
	Status          string    `json:"status"`
	Attempts        int       `json:"attempts"`
	MaxAttempts     int       `json:"max_attempts"`
	NextAttemptAt   time.Time `json:"next_attempt_at"`
	LastError       string    `json:"last_error,omitempty"`
	ResultJSON      string    `json:"result_json,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

type WASMPluginInvocation struct {
	ABIVersion string                 `json:"abi_version"`
	Event      string                 `json:"event"`
	JobID      string                 `json:"job_id"`
	Object     WASMPluginObject       `json:"object"`
	Workspace  WASMPluginWorkspace    `json:"workspace"`
	Config     map[string]string      `json:"config,omitempty"`
	Context    map[string]interface{} `json:"context,omitempty"`
}

type WASMPluginObject struct {
	Bucket         string            `json:"bucket"`
	Key            string            `json:"key"`
	Size           int64             `json:"size"`
	ContentType    string            `json:"content_type,omitempty"`
	ETag           string            `json:"etag,omitempty"`
	ChecksumSHA256 string            `json:"checksum_sha256,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	InputPath      string            `json:"input_path"`
}

type WASMPluginWorkspace struct {
	OutputDir string `json:"output_dir"`
}

type WASMPluginResult struct {
	ABIVersion     string                    `json:"abi_version"`
	Metadata       map[string]string         `json:"metadata,omitempty"`
	Tags           map[string]string         `json:"tags,omitempty"`
	Embeddings     []WASMPluginEmbedding     `json:"embeddings,omitempty"`
	DerivedObjects []WASMPluginDerivedObject `json:"derived_objects,omitempty"`
}

// WASMPluginEmbedding is the portable ABI representation returned by a
// plugin. Values are persisted separately from object metadata so vectors are
// not exposed through S3 headers or plugin job history.
type WASMPluginEmbedding struct {
	Kind         string            `json:"kind"`
	Model        string            `json:"model"`
	ModelVersion string            `json:"model_version"`
	Metric       string            `json:"metric"`
	Dimensions   int               `json:"dimensions"`
	Values       []float32         `json:"values,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type ObjectEmbedding struct {
	ID              string            `json:"id"`
	Bucket          string            `json:"bucket"`
	Key             string            `json:"key"`
	SourceChecksum  string            `json:"source_checksum,omitempty"`
	SourceUpdatedAt time.Time         `json:"source_updated_at,omitempty"`
	PluginID        string            `json:"plugin_id"`
	Kind            string            `json:"kind"`
	Model           string            `json:"model"`
	ModelVersion    string            `json:"model_version"`
	Metric          string            `json:"metric"`
	Dimensions      int               `json:"dimensions"`
	Ordinal         int               `json:"ordinal"`
	Values          []float32         `json:"values,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type EmbeddingSearchQuery struct {
	Bucket        string    `json:"bucket,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	Model         string    `json:"model"`
	ModelVersion  string    `json:"model_version,omitempty"`
	Metric        string    `json:"metric,omitempty"`
	Values        []float32 `json:"values"`
	Limit         int       `json:"limit,omitempty"`
	MinScore      *float64  `json:"min_score,omitempty"`
	MaxCandidates int       `json:"max_candidates,omitempty"`
}

type EmbeddingSearchResult struct {
	Embedding ObjectEmbedding `json:"embedding"`
	Score     float64         `json:"score"`
}

type VectorSearchCapabilities struct {
	Backend       string `json:"backend"`
	Engine        string `json:"engine"`
	ANN           bool   `json:"ann"`
	HNSWProfiles  int    `json:"hnsw_profiles"`
	MaxProfiles   int    `json:"max_profiles"`
	EFSearch      int    `json:"ef_search,omitempty"`
	MaxScanTuples int    `json:"max_scan_tuples,omitempty"`
}

type WASMPluginDerivedObject struct {
	Path        string            `json:"path"`
	Key         string            `json:"key,omitempty"`
	KeySuffix   string            `json:"key_suffix,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}
