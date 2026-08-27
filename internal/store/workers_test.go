package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestHookDeliveryClaimIsAtomic(t *testing.T) {
	s := openWorkerTestStore(t)
	ctx := context.Background()
	createWorkerTestHook(t, s)
	if err := s.CreateHookDelivery(ctx, domain.HookDelivery{ID: "atomic-delivery", HookID: "worker-hook", Event: domain.HookEventObjectCreated, Bucket: "images", Key: "demo.txt", PayloadJSON: `{}`, Status: domain.HookDeliveryStatusPending, NextAttemptAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateHookDelivery() error = %v", err)
	}

	var claimedCount atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, claimed, err := s.ClaimNextHookDelivery(ctx, time.Now().UTC().Add(time.Second))
			if err != nil {
				errs <- err
				return
			}
			if claimed {
				claimedCount.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ClaimNextHookDelivery() error = %v", err)
	}
	if got := claimedCount.Load(); got != 1 {
		t.Fatalf("claimed count = %d, want 1", got)
	}
}

func TestRecoverStaleDurableWork(t *testing.T) {
	s := openWorkerTestStore(t)
	ctx := context.Background()
	createWorkerTestHook(t, s)
	now := time.Now().UTC()
	stale := now.Add(-time.Hour).Format(time.RFC3339Nano)

	if err := s.CreateHookDelivery(ctx, domain.HookDelivery{ID: "stale-delivery", HookID: "worker-hook", Event: domain.HookEventObjectCreated, Bucket: "images", Key: "hook.txt", PayloadJSON: `{}`, Status: domain.HookDeliveryStatusRunning}); err != nil {
		t.Fatalf("CreateHookDelivery() error = %v", err)
	}
	if _, err := s.exec(ctx, `UPDATE hook_deliveries SET updated_at = ? WHERE id = ?`, stale, "stale-delivery"); err != nil {
		t.Fatalf("age hook delivery: %v", err)
	}
	if recovered, err := s.RecoverStaleHookDeliveries(ctx, now.Add(-time.Minute)); err != nil || recovered != 1 {
		t.Fatalf("RecoverStaleHookDeliveries() = %d, %v, want 1", recovered, err)
	}
	delivery, err := s.GetHookDelivery(ctx, "stale-delivery")
	if err != nil || delivery.Status != domain.HookDeliveryStatusPending {
		t.Fatalf("recovered delivery = %+v, %v", delivery, err)
	}
	claimedDelivery, claimed, err := s.ClaimNextHookDelivery(ctx, now.Add(time.Second))
	if err != nil || !claimed || claimedDelivery.ID != delivery.ID {
		t.Fatalf("ClaimNextHookDelivery() = %+v, %v, %v", claimedDelivery, claimed, err)
	}
	if err := s.TouchHookDelivery(ctx, claimedDelivery.ID); err != nil {
		t.Fatalf("TouchHookDelivery() error = %v", err)
	}

	replica := domain.ObjectReplica{Bucket: "images", Key: "replica.txt", ProviderAccountID: "target", Status: "running"}
	if err := s.UpsertObjectReplica(ctx, replica); err != nil {
		t.Fatalf("UpsertObjectReplica() error = %v", err)
	}
	if _, err := s.exec(ctx, `UPDATE object_replicas SET updated_at = ? WHERE bucket = ? AND key = ? AND provider_account_id = ?`, stale, replica.Bucket, replica.Key, replica.ProviderAccountID); err != nil {
		t.Fatalf("age object replica: %v", err)
	}
	if recovered, err := s.RecoverStaleObjectReplicas(ctx, now.Add(-time.Minute)); err != nil || recovered != 1 {
		t.Fatalf("RecoverStaleObjectReplicas() = %d, %v, want 1", recovered, err)
	}
	claimedReplica, claimed, err := s.ClaimNextObjectReplica(ctx)
	if err != nil || !claimed || claimedReplica.Key != replica.Key {
		t.Fatalf("ClaimNextObjectReplica() = %+v, %v, %v", claimedReplica, claimed, err)
	}
	if err := s.TouchObjectReplica(ctx, claimedReplica.Bucket, claimedReplica.Key, claimedReplica.ProviderAccountID); err != nil {
		t.Fatalf("TouchObjectReplica() error = %v", err)
	}

	job := domain.MigrationJob{ID: "stale-migration", Bucket: "images", SourceProviderID: "source", TargetProviderID: "target", Mode: domain.MigrationModeCopy, Status: domain.MigrationStatusRunning, TotalObjects: 4, ProcessedObjects: 2, SucceededObjects: 2, CurrentKey: "b.txt", StartedAt: now.Add(-2 * time.Hour)}
	if err := s.CreateMigrationJob(ctx, job); err != nil {
		t.Fatalf("CreateMigrationJob() error = %v", err)
	}
	if _, err := s.exec(ctx, `UPDATE migration_jobs SET updated_at = ? WHERE id = ?`, stale, job.ID); err != nil {
		t.Fatalf("age migration job: %v", err)
	}
	if recovered, err := s.RecoverStaleMigrationJobs(ctx, now.Add(-time.Minute)); err != nil || recovered != 1 {
		t.Fatalf("RecoverStaleMigrationJobs() = %d, %v, want 1", recovered, err)
	}
	recoveredJob, err := s.GetMigrationJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetMigrationJob() error = %v", err)
	}
	if recoveredJob.Status != domain.MigrationStatusPending || recoveredJob.ProcessedObjects != 0 || recoveredJob.CurrentKey != "" || recoveredJob.StartedAt.IsZero() {
		t.Fatalf("recovered migration = %+v", recoveredJob)
	}
	claimedJob, claimed, err := s.ClaimNextMigrationJob(ctx)
	if err != nil || !claimed || claimedJob.ID != recoveredJob.ID {
		t.Fatalf("ClaimNextMigrationJob() = %+v, %v, %v", claimedJob, claimed, err)
	}
	if err := s.TouchMigrationJob(ctx, claimedJob.ID); err != nil {
		t.Fatalf("TouchMigrationJob() error = %v", err)
	}
}

func openWorkerTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "workers.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

func createWorkerTestHook(t *testing.T, s *Store) {
	t.Helper()
	if err := s.UpsertHook(context.Background(), domain.Hook{ID: "worker-hook", Name: "Worker hook", Kind: domain.HookKindHTTP, URL: "https://example.com/hook", Method: "POST", Events: []string{domain.HookEventObjectCreated}, Enabled: true}); err != nil {
		t.Fatalf("UpsertHook() error = %v", err)
	}
}
