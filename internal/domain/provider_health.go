package domain

import "time"

const (
	ProviderHealthHealthy   = "healthy"
	ProviderHealthDegraded  = "degraded"
	ProviderHealthUnhealthy = "unhealthy"
	ProviderHealthDisabled  = "disabled"
)

type ProviderHealth struct {
	ProviderAccountID string    `json:"provider_account_id"`
	Status            string    `json:"status"`
	Message           string    `json:"message"`
	CheckedAt         time.Time `json:"checked_at"`
	LatencyMillis     int64     `json:"latency_millis"`
}
