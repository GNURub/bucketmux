package domain

import "time"

const (
	AlertTypeQuotaNearLimit     = "quota.near_limit"
	AlertTypeQuotaExhausted     = "quota.exhausted"
	AlertTypeCredentialsInvalid = "credentials.invalid"
	AlertTypeProviderDegraded   = "provider.degraded"
	AlertTypeReplicaFailed      = "replica.failed"
	AlertTypeWASMPluginFailed   = "wasm_plugin.failed"

	AlertSeverityWarning  = "warning"
	AlertSeverityCritical = "critical"
	AlertStatusOpen       = "open"
	AlertStatusResolved   = "resolved"
)

type Alert struct {
	ID                string    `json:"id"`
	DedupeKey         string    `json:"dedupe_key"`
	Type              string    `json:"type"`
	Severity          string    `json:"severity"`
	ProviderAccountID string    `json:"provider_account_id,omitempty"`
	Bucket            string    `json:"bucket,omitempty"`
	Key               string    `json:"key,omitempty"`
	Message           string    `json:"message"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	ResolvedAt        time.Time `json:"resolved_at,omitempty"`
}
