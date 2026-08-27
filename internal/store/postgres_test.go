package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestRebindPostgresPlaceholders(t *testing.T) {
	s := &Store{dialect: dialectPostgres}
	got := s.rebind("SELECT * FROM objects WHERE bucket = ? AND key > ? LIMIT ?")
	want := "SELECT * FROM objects WHERE bucket = $1 AND key > $2 LIMIT $3"
	if got != want {
		t.Fatalf("rebind() = %q, want %q", got, want)
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_POSTGRES_INTEGRATION") != "1" {
		t.Skip("set BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 to run")
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_DSN is required")
	}
	s, err := OpenPostgres(config.PostgresStoreConfig{DSN: dsn, MaxOpenConns: 1, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("OpenPostgres() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	ctx := context.Background()
	suffix := time.Now().UTC().Format("20060102150405")
	providerID := "pg-provider-" + suffix
	bucketName := "pg-bucket-" + suffix
	key := "objects/demo.txt"

	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: providerID, Name: providerID, Kind: domain.ProviderKindLocal, Bucket: bucketName, CapacityBytes: 1024, Enabled: true, Settings: map[string]string{"path": "/tmp"}}); err != nil {
		t.Fatalf("UpsertProvider() error = %v", err)
	}
	if err := s.UpsertBucket(ctx, domain.Bucket{Name: bucketName, ReplicationEnabled: true, ReplicationProviderIDs: []string{providerID}}); err != nil {
		t.Fatalf("UpsertBucket() error = %v", err)
	}
	bucket, err := s.GetBucket(ctx, bucketName)
	if err != nil || len(bucket.ReplicationProviderIDs) != 1 {
		t.Fatalf("GetBucket() = %+v, %v", bucket, err)
	}
	obj := domain.ObjectRecord{Bucket: bucketName, Key: key, ProviderAccountID: providerID, RemoteBucket: bucketName, RemoteKey: key, Size: 4, ReplicaStatus: "none"}
	if err := s.PutObject(ctx, obj); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	if _, err := s.GetObject(ctx, bucketName, key); err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	if joinedObject, joinedProvider, err := s.GetObjectWithProvider(ctx, bucketName, key); err != nil || joinedObject.Key != key || joinedProvider.ID != providerID {
		t.Fatalf("GetObjectWithProvider() = %+v, %+v, %v", joinedObject, joinedProvider, err)
	}
	if err := s.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: bucketName, Key: key, ProviderAccountID: providerID, Status: "pending"}); err != nil {
		t.Fatalf("UpsertObjectReplica() error = %v", err)
	}
	if replica, claimed, err := s.ClaimNextObjectReplica(ctx); err != nil || !claimed || replica.Key != key {
		t.Fatalf("ClaimNextObjectReplica() = %+v, %v, %v", replica, claimed, err)
	}
	if err := s.TouchObjectReplica(ctx, bucketName, key, providerID); err != nil {
		t.Fatalf("TouchObjectReplica() error = %v", err)
	}
	hookID := "pg-hook-" + suffix
	if err := s.UpsertHook(ctx, domain.Hook{ID: hookID, Name: hookID, Kind: domain.HookKindHTTP, URL: "https://example.com/hook", Method: "POST", Events: []string{domain.HookEventObjectCreated}, Enabled: true}); err != nil {
		t.Fatalf("UpsertHook() error = %v", err)
	}
	deliveryID := "pg-delivery-" + suffix
	if err := s.CreateHookDelivery(ctx, domain.HookDelivery{ID: deliveryID, HookID: hookID, Event: domain.HookEventObjectCreated, Bucket: bucketName, Key: key, PayloadJSON: `{}`, Status: domain.HookDeliveryStatusPending}); err != nil {
		t.Fatalf("CreateHookDelivery() error = %v", err)
	}
	if delivery, claimed, err := s.ClaimNextHookDelivery(ctx, time.Now().UTC().Add(time.Second)); err != nil || !claimed || delivery.ID != deliveryID || delivery.Status != domain.HookDeliveryStatusRunning {
		t.Fatalf("ClaimNextHookDelivery() = %+v, %v, %v", delivery, claimed, err)
	}
	if err := s.TouchHookDelivery(ctx, deliveryID); err != nil {
		t.Fatalf("TouchHookDelivery() error = %v", err)
	}
	jobID := "pg-migration-" + suffix
	if err := s.CreateMigrationJob(ctx, domain.MigrationJob{ID: jobID, Bucket: bucketName, SourceProviderID: providerID, TargetProviderID: providerID + "-target", Mode: domain.MigrationModeCopy, Status: domain.MigrationStatusPending}); err != nil {
		t.Fatalf("CreateMigrationJob() error = %v", err)
	}
	if job, claimed, err := s.ClaimNextMigrationJob(ctx); err != nil || !claimed || job.ID != jobID || job.Status != domain.MigrationStatusRunning {
		t.Fatalf("ClaimNextMigrationJob() = %+v, %v, %v", job, claimed, err)
	}
	if err := s.TouchMigrationJob(ctx, jobID); err != nil {
		t.Fatalf("TouchMigrationJob() error = %v", err)
	}
}
