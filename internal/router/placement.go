package router

import (
	"context"
	"errors"
	"sort"

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
		if leftFree != rightFree {
			return leftFree > rightFree
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

func remaining(p domain.ProviderAccount) int64 {
	if p.CapacityBytes <= 0 {
		return 1<<62 - 1
	}
	return p.CapacityBytes - p.UsedBytes
}

var ErrNoProviderAvailable = errors.New("no provider available")
