package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	s, err := OpenPostgres(config.PostgresStoreConfig{DSN: dsn, MaxOpenConns: 10, MaxIdleConns: 5})
	if err != nil {
		t.Fatalf("OpenPostgres() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
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

func TestPostgresAtomicQuotaAcrossInstances(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_POSTGRES_INTEGRATION") != "1" {
		t.Skip("set BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 to run")
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_DSN is required")
	}
	first, err := OpenPostgres(config.PostgresStoreConfig{DSN: dsn, MaxOpenConns: 10, MaxIdleConns: 5})
	if err != nil {
		t.Fatalf("OpenPostgres(first instance) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenPostgres(config.PostgresStoreConfig{DSN: dsn, MaxOpenConns: 10, MaxIdleConns: 5})
	if err != nil {
		t.Fatalf("OpenPostgres(second instance) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	quotaProviderID := "pg-quota-" + suffix
	if err := first.UpsertProvider(ctx, domain.ProviderAccount{ID: quotaProviderID, Name: quotaProviderID, Kind: domain.ProviderKindS3Compat, Bucket: "quota-test", CapacityBytes: 100, Enabled: true}); err != nil {
		t.Fatalf("UpsertProvider(quota) error = %v", err)
	}
	t.Cleanup(func() { _ = first.DeleteProvider(context.Background(), quotaProviderID) })
	var successes atomic.Int64
	var failures atomic.Int64
	var wait sync.WaitGroup
	for index := range 20 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			instance := first
			if index%2 == 1 {
				instance = second
			}
			ok, err := instance.ReserveProviderCapacity(ctx, domain.ProviderReservation{ID: fmt.Sprintf("pg-reservation-%s-%d", suffix, index), ProviderAccountID: quotaProviderID, Bytes: 10}, 0, 0, time.Now().UTC().Format("2006-01"))
			if err != nil {
				failures.Add(1)
				return
			}
			if ok {
				successes.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if failures.Load() != 0 || successes.Load() != 10 {
		t.Fatalf("multi-instance reservations successes=%d failures=%d", successes.Load(), failures.Load())
	}
	quotaAccount, err := second.GetProvider(ctx, quotaProviderID)
	if err != nil || quotaAccount.ReservedBytes != 100 {
		t.Fatalf("shared quota account=%+v err=%v", quotaAccount, err)
	}

	pluginID := "pg-wasm-" + suffix
	if err := first.UpsertWASMPlugin(ctx, domain.WASMPlugin{ID: pluginID, Name: pluginID, ABIVersion: domain.WASMPluginABIV1, ModuleBase64: "AGFzbQ==", Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "*", Enabled: true, TimeoutMillis: 1000, MemoryLimitBytes: 1 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 3}); err != nil {
		t.Fatalf("UpsertWASMPlugin() error = %v", err)
	}
	t.Cleanup(func() { _ = first.DeleteWASMPlugin(context.Background(), pluginID) })
	if created, err := first.CreateWASMPluginJob(ctx, domain.WASMPluginJob{ID: "pg-wasm-job-" + suffix, PluginID: pluginID, Event: domain.WASMPluginEventObjectCreated, Bucket: "images", Key: "photo.jpg", DedupeKey: "pg-wasm-dedupe-" + suffix, MaxAttempts: 3}); err != nil || !created {
		t.Fatalf("CreateWASMPluginJob() = %v, %v", created, err)
	}
	var pluginClaims atomic.Int64
	wait = sync.WaitGroup{}
	for index := range 20 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			instance := first
			if index%2 == 1 {
				instance = second
			}
			_, claimed, claimErr := instance.ClaimNextWASMPluginJob(ctx, time.Now().UTC().Add(time.Second))
			if claimErr != nil {
				failures.Add(1)
				return
			}
			if claimed {
				pluginClaims.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if pluginClaims.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("multi-instance WASM job claims=%d failures=%d, want 1/0", pluginClaims.Load(), failures.Load())
	}

	embeddingObject := domain.ObjectRecord{Bucket: "pg-embeddings-" + suffix, Key: "faces/alice.jpg", ProviderAccountID: quotaProviderID, RemoteBucket: "quota-test", RemoteKey: "faces/alice.jpg", Size: 3, ChecksumSHA256: "alice"}
	if err := first.PutObject(ctx, embeddingObject); err != nil {
		t.Fatalf("PutObject(embedding object) error = %v", err)
	}
	t.Cleanup(func() { _ = first.DeleteObject(context.Background(), embeddingObject.Bucket, embeddingObject.Key) })
	embeddingObject, err = first.GetObject(ctx, embeddingObject.Bucket, embeddingObject.Key)
	if err != nil {
		t.Fatalf("GetObject(embedding object) error = %v", err)
	}
	if err := first.ReplaceObjectEmbeddings(ctx, embeddingObject, pluginID, []domain.WASMPluginEmbedding{{Kind: "face", Model: "arcface", ModelVersion: "1", Metric: "cosine", Dimensions: 3, Values: []float32{1, 0, 0}}}); err != nil {
		t.Fatalf("ReplaceObjectEmbeddings(Postgres) error = %v", err)
	}
	sharedEmbeddings, err := second.ListObjectEmbeddings(ctx, embeddingObject.Bucket, embeddingObject.Key, true)
	if err != nil || len(sharedEmbeddings) != 1 || len(sharedEmbeddings[0].Values) != 3 {
		t.Fatalf("shared Postgres embeddings=%+v err=%v", sharedEmbeddings, err)
	}
	searchResults, err := second.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Bucket: embeddingObject.Bucket, Kind: "face", Model: "arcface", ModelVersion: "1", Values: []float32{1, 0, 0}, Limit: 1})
	if err != nil || len(searchResults) != 1 || searchResults[0].Embedding.Key != embeddingObject.Key || searchResults[0].Score < 0.999999 {
		t.Fatalf("shared Postgres embedding search=%+v err=%v", searchResults, err)
	}
	if os.Getenv("BUCKETMUX_EXPECT_PGVECTOR") == "1" {
		if err := first.ReplaceObjectEmbeddings(ctx, embeddingObject, "metric-plugin", []domain.WASMPluginEmbedding{
			{Kind: "face-dot", Model: "metric-model", ModelVersion: suffix, Metric: "dot", Dimensions: 3, Values: []float32{2, 0, 0}},
			{Kind: "face-l2", Model: "metric-model", ModelVersion: suffix, Metric: "l2", Dimensions: 3, Values: []float32{1, 1, 0}},
		}); err != nil {
			t.Fatalf("store pgvector metric fixtures: %v", err)
		}
		dotResults, dotErr := second.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Bucket: embeddingObject.Bucket, Kind: "face-dot", Model: "metric-model", ModelVersion: suffix, Metric: "dot", Values: []float32{1, 0, 0}, Limit: 1})
		if dotErr != nil || len(dotResults) != 1 || dotResults[0].Score != 2 {
			t.Fatalf("pgvector dot results=%+v err=%v", dotResults, dotErr)
		}
		l2Results, l2Err := second.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Bucket: embeddingObject.Bucket, Kind: "face-l2", Model: "metric-model", ModelVersion: suffix, Metric: "l2", Values: []float32{1, 0, 0}, Limit: 1})
		if l2Err != nil || len(l2Results) != 1 || l2Results[0].Score != -1 {
			t.Fatalf("pgvector l2 results=%+v err=%v", l2Results, l2Err)
		}
		capabilities, err := second.VectorSearchCapabilities(ctx)
		if err != nil || capabilities.Backend != "pgvector" || !capabilities.ANN || capabilities.HNSWProfiles < 1 {
			t.Fatalf("pgvector capabilities=%+v err=%v", capabilities, err)
		}
		if _, err := first.exec(ctx, `DELETE FROM object_embedding_vectors WHERE embedding_id = ?`, sharedEmbeddings[0].ID); err != nil {
			t.Fatalf("delete pgvector backfill fixture: %v", err)
		}
		if err := second.backfillPGVectors(ctx); err != nil {
			t.Fatalf("backfillPGVectors() error = %v", err)
		}
		var backfilled int
		if err := second.queryRow(ctx, `SELECT COUNT(*) FROM object_embedding_vectors WHERE embedding_id = ?`, sharedEmbeddings[0].ID).Scan(&backfilled); err != nil || backfilled != 1 {
			t.Fatalf("backfilled vector count=%d err=%v", backfilled, err)
		}

		concurrentObjects := []domain.ObjectRecord{
			{Bucket: "pg-embeddings-" + suffix, Key: "faces/concurrent-a.jpg", ProviderAccountID: quotaProviderID, RemoteBucket: "quota-test", RemoteKey: "faces/concurrent-a.jpg", Size: 3},
			{Bucket: "pg-embeddings-" + suffix, Key: "faces/concurrent-b.jpg", ProviderAccountID: quotaProviderID, RemoteBucket: "quota-test", RemoteKey: "faces/concurrent-b.jpg", Size: 3},
		}
		for i := range concurrentObjects {
			if err := first.PutObject(ctx, concurrentObjects[i]); err != nil {
				t.Fatal(err)
			}
			concurrentObjects[i], err = first.GetObject(ctx, concurrentObjects[i].Bucket, concurrentObjects[i].Key)
			if err != nil {
				t.Fatal(err)
			}
			deleting := concurrentObjects[i]
			t.Cleanup(func() { _ = first.DeleteObject(context.Background(), deleting.Bucket, deleting.Key) })
		}
		writeErrors := make(chan error, 2)
		for i, object := range concurrentObjects {
			instance := first
			if i == 1 {
				instance = second
			}
			go func(instance *Store, object domain.ObjectRecord, ordinal int) {
				writeErrors <- instance.ReplaceObjectEmbeddings(ctx, object, pluginID, []domain.WASMPluginEmbedding{{Kind: "face", Model: "arcface-concurrent", ModelVersion: suffix, Metric: "cosine", Dimensions: 3, Values: []float32{1, float32(ordinal), 0}}})
			}(instance, object, i)
		}
		for range 2 {
			if err := <-writeErrors; err != nil {
				t.Fatalf("concurrent pgvector profile creation: %v", err)
			}
		}
	}
}
