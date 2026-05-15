package provider

import (
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func healthy(account domain.ProviderAccount, started time.Time, message string) domain.ProviderHealth {
	return providerHealth(account, started, domain.ProviderHealthHealthy, message)
}

func degraded(account domain.ProviderAccount, started time.Time, message string) domain.ProviderHealth {
	return providerHealth(account, started, domain.ProviderHealthDegraded, message)
}

func unhealthy(account domain.ProviderAccount, started time.Time, format string, args ...any) domain.ProviderHealth {
	return providerHealth(account, started, domain.ProviderHealthUnhealthy, fmt.Sprintf(format, args...))
}

func providerHealth(account domain.ProviderAccount, started time.Time, status, message string) domain.ProviderHealth {
	now := time.Now().UTC()
	return domain.ProviderHealth{
		ProviderAccountID: account.ID,
		Status:            status,
		Message:           message,
		CheckedAt:         now,
		LatencyMillis:     now.Sub(started).Milliseconds(),
	}
}
