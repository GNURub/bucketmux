package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Service) ListProviderHealth(ctx context.Context) ([]domain.ProviderHealth, error) {
	providers, err := s.Store.ListProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProviderHealth, 0, len(providers))
	for _, account := range providers {
		out = append(out, s.CheckProviderHealth(ctx, account))
	}
	return out, nil
}

func (s *Service) CheckProviderHealth(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	if !account.Enabled {
		return providerHealth(account, started, domain.ProviderHealthDisabled, "provider is disabled")
	}
	account, err := s.decryptAccount(account)
	if err != nil {
		return providerHealth(account, started, domain.ProviderHealthUnhealthy, err.Error())
	}
	adapter, ok := s.Providers.Get(account.Kind)
	if !ok {
		return providerHealth(account, started, domain.ProviderHealthUnhealthy, fmt.Sprintf("provider kind %s is not registered", account.Kind))
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return adapter.Health(checkCtx, account)
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
