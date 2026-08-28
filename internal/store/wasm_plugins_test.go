package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestWASMPluginJobClaimIsAtomicAndDeduplicated(t *testing.T) {
	db := openWorkerTestStore(t)
	ctx := context.Background()
	plugin := domain.WASMPlugin{ID: "classifier", Name: "Classifier", ABIVersion: domain.WASMPluginABIV1, ModuleBase64: "AGFzbQ==", Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "*", Enabled: true, TimeoutMillis: 1000, MemoryLimitBytes: 1 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 3}
	if err := db.UpsertWASMPlugin(ctx, plugin); err != nil {
		t.Fatalf("UpsertWASMPlugin() error = %v", err)
	}
	job := domain.WASMPluginJob{ID: "job-1", PluginID: plugin.ID, Event: domain.WASMPluginEventObjectCreated, Bucket: "images", Key: "a.jpg", DedupeKey: "dedupe", MaxAttempts: 3}
	created, err := db.CreateWASMPluginJob(ctx, job)
	if err != nil || !created {
		t.Fatalf("CreateWASMPluginJob() = %v, %v", created, err)
	}
	job.ID = "job-2"
	if created, err := db.CreateWASMPluginJob(ctx, job); err != nil || created {
		t.Fatalf("duplicate CreateWASMPluginJob() = %v, %v", created, err)
	}

	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, claimed, err := db.ClaimNextWASMPluginJob(ctx, time.Now().UTC().Add(time.Second))
			if err != nil {
				t.Errorf("ClaimNextWASMPluginJob() error = %v", err)
				return
			}
			if claimed {
				claims.Add(1)
			}
		})
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claims = %d, want 1", claims.Load())
	}
	claimed, err := db.GetWASMPluginJob(ctx, "job-1")
	if err != nil || claimed.Attempts != 1 || claimed.Status != domain.WASMPluginStatusRunning {
		t.Fatalf("claimed job = %+v, %v", claimed, err)
	}
}
