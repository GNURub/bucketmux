package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestRepairJobsClaimProgressAndRecover(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.UpsertBucket(t.Context(), domain.Bucket{Name: "images"}); err != nil {
		t.Fatal(err)
	}
	job := domain.RepairJob{ID: "repair-1", Bucket: "images", Prefix: "photos/"}
	if err := db.CreateRepairJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := db.ClaimNextRepairJob(t.Context())
	if err != nil || !ok || claimed.Status != domain.RepairStatusRunning {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := db.ClaimNextRepairJob(t.Context()); err != nil || ok {
		t.Fatalf("duplicate claim ok=%v err=%v", ok, err)
	}
	claimed.CheckedObjects = 4
	claimed.RepairedObjects = 1
	if err := db.UpdateRepairJob(t.Context(), claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(t.Context(), `UPDATE repair_jobs SET updated_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), claimed.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.RecoverStaleRepairJobs(t.Context(), time.Now().UTC().Add(-time.Minute))
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	claimed, ok, err = db.ClaimNextRepairJob(t.Context())
	if err != nil || !ok || claimed.CheckedObjects != 4 || claimed.RepairedObjects != 1 {
		t.Fatalf("reclaim=%+v ok=%v err=%v", claimed, ok, err)
	}
}
