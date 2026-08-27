package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestLocalInventoryImportAndReconcile(t *testing.T) {
	dataDir := t.TempDir()
	providerRoot := filepath.Join(dataDir, "external")
	objectPath := filepath.Join(providerRoot, "archive", "existing", "report.txt")
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, []byte("already-there"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(context.Background(), config.Config{
		Server:    config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "store.db"), MasterKey: "inventory-master"},
		S3:        config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Providers: []config.ProviderConfig{{ID: "external-local", Name: "External", Kind: string(domain.ProviderKindLocal), Bucket: "archive", Priority: 1, Enabled: new(true), Settings: map[string]string{"path": providerRoot}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	health, err := svc.TestProviderConnection(t.Context(), "external-local")
	if err != nil || health.Status != domain.ProviderHealthHealthy {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	buckets, err := svc.DiscoverProviderBuckets(t.Context(), "external-local")
	if err != nil || len(buckets) != 1 || buckets[0].Name != "archive" {
		t.Fatalf("buckets=%+v err=%v", buckets, err)
	}
	job, err := svc.CreateInventoryJob(t.Context(), CreateInventoryJobInput{ProviderAccountID: "external-local", Bucket: "archive", Mode: domain.InventoryModeImport})
	if err != nil {
		t.Fatal(err)
	}
	job = waitInventoryJob(t, svc, job.ID)
	if job.Status != domain.InventoryStatusCompleted || job.DiscoveredObjects != 1 || job.ImportedObjects != 1 {
		t.Fatalf("import job=%+v", job)
	}
	body, _, err := svc.GetObject(t.Context(), "archive", "existing/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	mapped, err := svc.CreateInventoryJob(t.Context(), CreateInventoryJobInput{ProviderAccountID: "external-local", Bucket: "catalog", RemoteBucket: "archive", Mode: domain.InventoryModeImport})
	if err != nil {
		t.Fatal(err)
	}
	mapped = waitInventoryJob(t, svc, mapped.ID)
	mappedObject, err := svc.Store.GetObject(t.Context(), "catalog", "existing/report.txt")
	if err != nil || mapped.Status != domain.InventoryStatusCompleted || mapped.RemoteBucket != "archive" || mappedObject.RemoteBucket != "archive" {
		t.Fatalf("mapped job=%+v object=%+v err=%v", mapped, mappedObject, err)
	}

	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	reconcile, err := svc.CreateInventoryJob(t.Context(), CreateInventoryJobInput{ProviderAccountID: "external-local", Bucket: "archive", Mode: domain.InventoryModeReconcile})
	if err != nil {
		t.Fatal(err)
	}
	reconcile = waitInventoryJob(t, svc, reconcile.ID)
	if reconcile.Status != domain.InventoryStatusCompleted || reconcile.MissingObjects != 1 {
		t.Fatalf("reconcile job=%+v", reconcile)
	}
}

func TestPlacementPlannerAndCostOptimization(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	ctx := t.Context()
	for _, provider := range []domain.ProviderAccount{
		{ID: "expensive", Name: "Expensive", Kind: domain.ProviderKindLocal, Bucket: "images", UsedBytes: 10 << 30, CapacityBytes: 20 << 30, Priority: 10, Enabled: true, Settings: map[string]string{"cost_per_gb_month": "0.025"}},
		{ID: "cheap", Name: "Cheap", Kind: domain.ProviderKindLocal, Bucket: "images", CapacityBytes: 30 << 30, Priority: 10, Enabled: true, Settings: map[string]string{"cost_per_gb_month": "0.005"}},
	} {
		if err := svc.Store.UpsertProvider(ctx, provider); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := svc.PlanPlacement(ctx, "images", 1<<30)
	if err != nil || len(plan.Providers) < 2 || !plan.Providers[0].Recommended || plan.Providers[0].ProviderAccountID != "cheap" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	optimizations, err := svc.CostOptimizations(ctx)
	if err != nil || len(optimizations) == 0 || optimizations[0].SourceProviderID != "expensive" || optimizations[0].TargetProviderID != "cheap" || optimizations[0].EstimatedMonthlySaving < 0.19 {
		t.Fatalf("optimizations=%+v err=%v", optimizations, err)
	}
}

func TestLifecycleExpiresAndPurgesProtectedObject(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	ctx := t.Context()
	bucket, err := svc.Store.GetBucket(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	bucket.TrashEnabled = true
	bucket.TrashRetentionDays = 1
	bucket.LifecycleRules = []domain.LifecycleRule{{ID: "expire-logs", Prefix: "logs/", ExpireAfterDays: 1, Enabled: true}}
	if err := svc.Store.UpsertBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutObject(ctx, domain.PutObjectInput{Bucket: "images", Key: "logs/old.txt", Size: 3, ContentType: "text/plain"}, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	result, err := svc.RunLifecycleOnce(ctx, time.Now().UTC().Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiredObjects != 1 || result.PurgedTrash != 1 || result.Failures != 0 {
		t.Fatalf("lifecycle result=%+v", result)
	}
	if _, err := svc.Store.GetObject(ctx, "images", "logs/old.txt"); err == nil {
		t.Fatal("expired object remains indexed")
	}
	trash, err := svc.Store.ListTrashObjects(ctx, 10)
	if err != nil || len(trash) != 0 {
		t.Fatalf("trash=%+v err=%v", trash, err)
	}
}

func TestBootstrapPreservesUIProtectionUnlessConfigured(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	ctx := t.Context()
	bucket, err := svc.Store.GetBucket(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	bucket.VersioningEnabled = true
	bucket.TrashEnabled = true
	bucket.TrashRetentionDays = 14
	if err := svc.Store.UpsertBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if err := svc.Bootstrap(ctx, config.Config{Buckets: []config.BucketConfig{{Name: "images"}}}); err != nil {
		t.Fatal(err)
	}
	bucket, err = svc.Store.GetBucket(ctx, "images")
	if err != nil || !bucket.VersioningEnabled || !bucket.TrashEnabled || bucket.TrashRetentionDays != 14 {
		t.Fatalf("preserved bucket=%+v err=%v", bucket, err)
	}
	disabled := false
	seven := 7
	if err := svc.Bootstrap(ctx, config.Config{Buckets: []config.BucketConfig{{Name: "images", VersioningEnabled: &disabled, TrashRetentionDays: &seven}}}); err != nil {
		t.Fatal(err)
	}
	bucket, err = svc.Store.GetBucket(ctx, "images")
	if err != nil || bucket.VersioningEnabled || bucket.TrashRetentionDays != 7 {
		t.Fatalf("configured bucket=%+v err=%v", bucket, err)
	}
}

func TestLifecycleRulePurgesTrashBeforeBucketDefault(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	ctx := t.Context()
	bucket, err := svc.Store.GetBucket(ctx, "images")
	if err != nil {
		t.Fatal(err)
	}
	bucket.TrashEnabled = true
	bucket.TrashRetentionDays = 365
	bucket.LifecycleRules = []domain.LifecycleRule{{ID: "short-trash", Prefix: "tmp/", PurgeTrashAfterDays: 1, Enabled: true}}
	if err := svc.Store.UpsertBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutObject(ctx, domain.PutObjectInput{Bucket: "images", Key: "tmp/delete.txt", Size: 4}, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteObject(ctx, "images", "tmp/delete.txt"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.RunLifecycleOnce(ctx, time.Now().UTC().Add(2*24*time.Hour))
	if err != nil || result.PurgedTrash != 1 {
		t.Fatalf("lifecycle result=%+v err=%v", result, err)
	}
	trash, err := svc.Store.ListTrashObjects(ctx, 10)
	if err != nil || len(trash) != 0 {
		t.Fatalf("trash=%+v err=%v", trash, err)
	}
}

func waitInventoryJob(t *testing.T, svc *Service, id string) domain.InventoryJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := svc.Store.GetInventoryJob(t.Context(), id)
		if err == nil && (job.Status == domain.InventoryStatusCompleted || job.Status == domain.InventoryStatusFailed) {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := svc.Store.GetInventoryJob(t.Context(), id)
	t.Fatalf("inventory job did not finish: %+v", job)
	return domain.InventoryJob{}
}
