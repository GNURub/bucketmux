package router

import (
	"context"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

type fakeProviderStore struct{ providers []domain.ProviderAccount }

func (f fakeProviderStore) ListProviders(ctx context.Context, enabledOnly bool) ([]domain.ProviderAccount, error) {
	var out []domain.ProviderAccount
	for _, p := range f.providers {
		if enabledOnly && !p.Enabled {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func TestPlacementRouterChoose(t *testing.T) {
	tests := []struct {
		name      string
		inputSize int64
		providers []domain.ProviderAccount
		wantID    string
		wantErr   bool
	}{
		{
			name:      "chooses lowest priority when capacity fits",
			inputSize: 10,
			providers: []domain.ProviderAccount{
				{ID: "slow", Priority: 100, CapacityBytes: 100, UsedBytes: 0, Enabled: true},
				{ID: "fast", Priority: 10, CapacityBytes: 100, UsedBytes: 0, Enabled: true},
			},
			wantID: "fast",
		},
		{
			name:      "skips full provider",
			inputSize: 20,
			providers: []domain.ProviderAccount{
				{ID: "full", Priority: 1, CapacityBytes: 100, UsedBytes: 90, Enabled: true},
				{ID: "roomy", Priority: 10, CapacityBytes: 100, UsedBytes: 0, Enabled: true},
			},
			wantID: "roomy",
		},
		{
			name:      "ignores disabled providers",
			inputSize: 1,
			providers: []domain.ProviderAccount{
				{ID: "disabled", Priority: 1, Enabled: false},
			},
			wantErr: true,
		},
		{
			name:      "skips provider policy max object size",
			inputSize: 20,
			providers: []domain.ProviderAccount{
				{ID: "small-only", Priority: 1, CapacityBytes: 100, Enabled: true, Settings: map[string]string{"max_object_size_bytes": "10"}},
				{ID: "large-ok", Priority: 10, CapacityBytes: 100, Enabled: true},
			},
			wantID: "large-ok",
		},
		{
			name:      "uses lower cost as priority tie breaker",
			inputSize: 10,
			providers: []domain.ProviderAccount{
				{ID: "expensive", Priority: 1, CapacityBytes: 100, Enabled: true, Settings: map[string]string{"cost_per_gb_month": "9"}},
				{ID: "cheap", Priority: 1, CapacityBytes: 100, Enabled: true, Settings: map[string]string{"cost_per_gb_month": "1"}},
			},
			wantID: "cheap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewPlacementRouter(fakeProviderStore{providers: tt.providers})
			got, err := r.Choose(context.Background(), domain.PutObjectInput{Size: tt.inputSize}, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.ID != tt.wantID {
				t.Fatalf("provider = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}
