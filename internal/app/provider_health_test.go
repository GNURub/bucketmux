package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestListProviderHealthLocal(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:   config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "local-health",
			Name:          "Local health",
			Kind:          string(domain.ProviderKindLocal),
			Bucket:        "images",
			CapacityBytes: 1024 * 1024,
			Priority:      1,
			Enabled:       new(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	health, err := svc.ListProviderHealth(context.Background())
	if err != nil {
		t.Fatalf("ListProviderHealth() error = %v", err)
	}
	if len(health) != 1 {
		t.Fatalf("len(health) = %d, want 1", len(health))
	}
	if health[0].ProviderAccountID != "local-health" || health[0].Status != domain.ProviderHealthHealthy || health[0].Message == "" {
		t.Fatalf("health[0] = %+v", health[0])
	}
}

func TestCheckProviderHealthDisabled(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()

	health := svc.CheckProviderHealth(context.Background(), domain.ProviderAccount{ID: "disabled", Kind: domain.ProviderKindLocal, Enabled: false})
	if health.Status != domain.ProviderHealthDisabled || health.ProviderAccountID != "disabled" {
		t.Fatalf("health = %+v", health)
	}
}
