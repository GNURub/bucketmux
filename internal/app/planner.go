package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const bytesPerGiB = 1024 * 1024 * 1024

type ProviderPlacementPlan struct {
	ProviderAccountID    string  `json:"provider_account_id"`
	ProviderName         string  `json:"provider_name"`
	Eligible             bool    `json:"eligible"`
	Reason               string  `json:"reason,omitempty"`
	Priority             int     `json:"priority"`
	UsedBytes            int64   `json:"used_bytes"`
	ReservedBytes        int64   `json:"reserved_bytes"`
	CapacityBytes        int64   `json:"capacity_bytes"`
	RemainingBytes       int64   `json:"remaining_bytes"`
	CostPerGiBMonth      float64 `json:"cost_per_gib_month"`
	CurrentMonthlyCost   float64 `json:"current_monthly_cost"`
	ProjectedMonthlyCost float64 `json:"projected_monthly_cost"`
	Recommended          bool    `json:"recommended"`
}

type PlacementPlan struct {
	Bucket     string                  `json:"bucket"`
	ObjectSize int64                   `json:"object_size"`
	Providers  []ProviderPlacementPlan `json:"providers"`
}

type CostOptimization struct {
	SourceProviderID       string  `json:"source_provider_id"`
	TargetProviderID       string  `json:"target_provider_id"`
	Bytes                  int64   `json:"bytes"`
	EstimatedMonthlySaving float64 `json:"estimated_monthly_saving"`
}

func (s *Service) PlanPlacement(ctx context.Context, bucket string, size int64) (PlacementPlan, error) {
	providers, err := s.Store.ListProviders(ctx, false)
	if err != nil {
		return PlacementPlan{}, err
	}
	plan := PlacementPlan{Bucket: bucket, ObjectSize: size}
	for _, provider := range providers {
		cost := floatSetting(provider.Settings, "cost_per_gb_month")
		remaining := int64(^uint64(0) >> 1)
		if provider.CapacityBytes > 0 {
			remaining = provider.CapacityBytes - provider.UsedBytes - provider.ReservedBytes
		}
		if provider.RemoteCapacityBytes > 0 {
			remoteRemaining := provider.RemoteCapacityBytes - provider.RemoteUsedBytes - provider.ReservedBytes
			if remoteRemaining < remaining {
				remaining = remoteRemaining
			}
		}
		candidate := ProviderPlacementPlan{ProviderAccountID: provider.ID, ProviderName: provider.Name, Eligible: true, Priority: provider.Priority, UsedBytes: provider.UsedBytes, ReservedBytes: provider.ReservedBytes, CapacityBytes: provider.CapacityBytes, RemainingBytes: remaining, CostPerGiBMonth: cost, CurrentMonthlyCost: float64(provider.UsedBytes) / bytesPerGiB * cost, ProjectedMonthlyCost: float64(provider.UsedBytes+size) / bytesPerGiB * cost}
		switch {
		case !provider.Enabled:
			candidate.Eligible, candidate.Reason = false, "provider is disabled"
		case provider.AvailabilityStatus != "" && (provider.UnavailableUntil.IsZero() || provider.UnavailableUntil.After(time.Now().UTC())):
			candidate.Eligible, candidate.Reason = false, "provider is temporarily unavailable: "+provider.AvailabilityStatus
		case remaining < size:
			candidate.Eligible, candidate.Reason = false, "insufficient capacity"
		case intSetting(provider.Settings, "max_object_size_bytes") > 0 && size > intSetting(provider.Settings, "max_object_size_bytes"):
			candidate.Eligible, candidate.Reason = false, "object exceeds provider size policy"
		case intSetting(provider.Settings, "min_free_bytes") > 0 && remaining-size < intSetting(provider.Settings, "min_free_bytes"):
			candidate.Eligible, candidate.Reason = false, "minimum free capacity policy would be violated"
		}
		plan.Providers = append(plan.Providers, candidate)
	}
	sort.SliceStable(plan.Providers, func(i, j int) bool {
		left, right := plan.Providers[i], plan.Providers[j]
		if left.Eligible != right.Eligible {
			return left.Eligible
		}
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		if left.CostPerGiBMonth != right.CostPerGiBMonth {
			return left.CostPerGiBMonth < right.CostPerGiBMonth
		}
		return left.RemainingBytes > right.RemainingBytes
	})
	for index := range plan.Providers {
		if plan.Providers[index].Eligible {
			plan.Providers[index].Recommended = true
			break
		}
	}
	return plan, nil
}

func (s *Service) CostOptimizations(ctx context.Context) ([]CostOptimization, error) {
	providers, err := s.Store.ListProviders(ctx, true)
	if err != nil {
		return nil, err
	}
	var result []CostOptimization
	for _, source := range providers {
		sourceCost := floatSetting(source.Settings, "cost_per_gb_month")
		if source.UsedBytes <= 0 || sourceCost <= 0 {
			continue
		}
		var best *domain.ProviderAccount
		for index := range providers {
			target := &providers[index]
			targetCost := floatSetting(target.Settings, "cost_per_gb_month")
			if target.ID == source.ID || targetCost >= sourceCost || (target.CapacityBytes > 0 && target.CapacityBytes-target.UsedBytes < source.UsedBytes) {
				continue
			}
			if best == nil || targetCost < floatSetting(best.Settings, "cost_per_gb_month") {
				best = target
			}
		}
		if best != nil {
			saving := float64(source.UsedBytes) / bytesPerGiB * (sourceCost - floatSetting(best.Settings, "cost_per_gb_month"))
			result = append(result, CostOptimization{SourceProviderID: source.ID, TargetProviderID: best.ID, Bytes: source.UsedBytes, EstimatedMonthlySaving: saving})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EstimatedMonthlySaving > result[j].EstimatedMonthlySaving })
	return result, nil
}

func floatSetting(settings map[string]string, key string) float64 {
	parsed, _ := strconv.ParseFloat(strings.TrimSpace(settings[key]), 64)
	if parsed < 0 {
		return 0
	}
	return parsed
}

func intSetting(settings map[string]string, key string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(settings[key]), 10, 64)
	if parsed < 0 {
		return 0
	}
	return parsed
}
