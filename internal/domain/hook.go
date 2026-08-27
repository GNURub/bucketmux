package domain

import "time"

type HookKind string

const (
	HookKindHTTP HookKind = "http"
)

const (
	HookEventObjectCreated = "object.created"
	HookEventObjectDeleted = "object.deleted"
)

type Hook struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Kind             HookKind          `json:"kind"`
	URL              string            `json:"url"`
	Method           string            `json:"method"`
	Events           []string          `json:"events"`
	Enabled          bool              `json:"enabled"`
	Headers          map[string]string `json:"-"`
	HeaderNames      []string          `json:"header_names,omitempty"`
	HeadersEncrypted string            `json:"-"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

const (
	HookDeliveryStatusPending   = "pending"
	HookDeliveryStatusRunning   = "running"
	HookDeliveryStatusSucceeded = "succeeded"
	HookDeliveryStatusFailed    = "failed"
)

type HookDelivery struct {
	ID             string    `json:"id"`
	HookID         string    `json:"hook_id"`
	Event          string    `json:"event"`
	Bucket         string    `json:"bucket"`
	Key            string    `json:"key"`
	PayloadJSON    string    `json:"payload_json,omitempty"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	MaxAttempts    int       `json:"max_attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	LastStatusCode int       `json:"last_status_code,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type BucketNotification struct {
	ID     string `json:"id"`
	Bucket string `json:"bucket"`
	HookID string `json:"hook_id"`
	Event  string `json:"event"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}
