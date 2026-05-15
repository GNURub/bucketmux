package router

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
)

type ProviderLister interface {
	ListProviders(ctx context.Context, enabledOnly bool) ([]domain.ProviderAccount, error)
}

type PlacementRouter struct {
	store ProviderLister
}

func NewPlacementRouter(store ProviderLister) *PlacementRouter {
	return &PlacementRouter{store: store}
}

func (r *PlacementRouter) Choose(ctx context.Context, input domain.PutObjectInput, exclude map[string]bool) (domain.ProviderAccount, error) {
	providers, err := r.store.ListProviders(ctx, true)
	if err != nil {
		return domain.ProviderAccount{}, err
	}
	var candidates []domain.ProviderAccount
	for _, p := range providers {
		if exclude != nil && exclude[p.ID] {
			continue
		}
		if p.CapacityBytes > 0 && p.UsedBytes+input.Size > p.CapacityBytes {
			continue
		}
		if violatesProviderPolicy(p, input) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return domain.ProviderAccount{}, ErrNoProviderAvailable
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftFree := remaining(candidates[i])
		rightFree := remaining(candidates[j])
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		leftCost := providerCost(candidates[i])
		rightCost := providerCost(candidates[j])
		if leftCost != rightCost {
			return leftCost < rightCost
		}
		if leftFree != rightFree {
			return leftFree > rightFree
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

func violatesProviderPolicy(p domain.ProviderAccount, input domain.PutObjectInput) bool {
	if maxObjectSize := int64Setting(p, "max_object_size_bytes"); maxObjectSize > 0 && input.Size > maxObjectSize {
		return true
	}
	if minFree := int64Setting(p, "min_free_bytes"); minFree > 0 && remaining(p)-input.Size < minFree {
		return true
	}
	return false
}

func providerCost(p domain.ProviderAccount) float64 {
	value := strings.TrimSpace(p.Settings["cost_per_gb_month"])
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func int64Setting(p domain.ProviderAccount, key string) int64 {
	value := strings.TrimSpace(p.Settings[key])
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func remaining(p domain.ProviderAccount) int64 {
	if p.CapacityBytes <= 0 {
		return 1<<62 - 1
	}
	return p.CapacityBytes - p.UsedBytes
}

var ErrNoProviderAvailable = errors.New("no provider available")
