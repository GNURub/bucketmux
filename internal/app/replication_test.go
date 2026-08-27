package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestPutObjectReplicatesToSelectedBucketProviders(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{
		Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:     config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:  config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{
			Name:                   "images",
			ReplicationProviderIDs: []string{"local-replica"},
		}},
		Providers: []config.ProviderConfig{
			{
				ID:            "local-primary",
				Name:          "Local primary",
				Kind:          string(domain.ProviderKindLocal),
				Bucket:        "images",
				CapacityBytes: 1024 * 1024,
				Priority:      1,
				Enabled:       new(true),
				Settings:      map[string]string{"path": filepath.Join(dataDir, "primary")},
			},
			{
				ID:            "local-replica",
				Name:          "Local replica",
				Kind:          string(domain.ProviderKindLocal),
				Bucket:        "images",
				CapacityBytes: 1024 * 1024,
				Priority:      100,
				Enabled:       new(true),
				Settings:      map[string]string{"path": filepath.Join(dataDir, "replica")},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if svc.cancelWorkers != nil {
		svc.cancelWorkers()
		svc.workerWG.Wait()
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	obj, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "photos/demo.txt", Size: int64(len("replicated")), ContentType: "text/plain"}, strings.NewReader("replicated"))
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	if obj.ProviderAccountID != "local-primary" || obj.ReplicaStatus != "pending" {
		t.Fatalf("object = %+v", obj)
	}
	replicas, err := svc.Store.ListObjectReplicas(context.Background(), "images", "photos/demo.txt")
	if err != nil {
		t.Fatalf("ListObjectReplicas() error = %v", err)
	}
	if len(replicas) != 1 || replicas[0].ProviderAccountID != "local-replica" || replicas[0].Status != "pending" {
		t.Fatalf("replicas = %+v", replicas)
	}
	if err := svc.RunObjectReplication(context.Background(), "images", "photos/demo.txt", "local-replica"); err != nil {
		t.Fatalf("RunObjectReplication() error = %v", err)
	}
	obj, err = svc.Store.GetObject(context.Background(), "images", "photos/demo.txt")
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	if obj.ReplicaStatus != "completed" {
		t.Fatalf("replica status = %s, want completed", obj.ReplicaStatus)
	}
	replicas, err = svc.Store.ListObjectReplicas(context.Background(), "images", "photos/demo.txt")
	if err != nil {
		t.Fatalf("ListObjectReplicas(after run) error = %v", err)
	}
	if len(replicas) != 1 || replicas[0].ProviderAccountID != "local-replica" || replicas[0].Status != "succeeded" {
		t.Fatalf("replicas after run = %+v", replicas)
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "replica", "images", "photos", "demo.txt"))
	if err != nil {
		t.Fatalf("ReadFile(replica) error = %v", err)
	}
	if string(data) != "replicated" {
		t.Fatalf("replica data = %q", string(data))
	}
	if err := os.Remove(filepath.Join(dataDir, "primary", "images", "photos", "demo.txt")); err != nil {
		t.Fatalf("remove primary fixture: %v", err)
	}
	body, served, err := svc.GetObject(context.Background(), "images", "photos/demo.txt")
	if err != nil {
		t.Fatalf("GetObject() with unavailable primary error = %v", err)
	}
	fallbackData, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || string(fallbackData) != "replicated" || served.ProviderAccountID != "local-replica" {
		t.Fatalf("replica fallback body=%q object=%+v readErr=%v", fallbackData, served, readErr)
	}
	head, err := svc.HeadObject(context.Background(), "images", "photos/demo.txt")
	if err != nil || head.ProviderAccountID != "local-replica" || head.Size != int64(len("replicated")) {
		t.Fatalf("HeadObject() replica fallback = %+v, %v", head, err)
	}
	repair, err := svc.RepairObject(context.Background(), "images", "photos/demo.txt")
	if err != nil || !repair.Repaired {
		t.Fatalf("RepairObject() = %+v, %v", repair, err)
	}
	primaryData, err := os.ReadFile(filepath.Join(dataDir, "primary", "images", "photos", "demo.txt"))
	if err != nil || string(primaryData) != "replicated" {
		t.Fatalf("repaired primary=%q err=%v", primaryData, err)
	}
	if err := os.Remove(filepath.Join(dataDir, "primary", "images", "photos", "demo.txt")); err != nil {
		t.Fatalf("remove repaired primary fixture: %v", err)
	}
	job, err := svc.CreateRepairJob(context.Background(), "images", "photos/")
	if err != nil {
		t.Fatalf("CreateRepairJob() error = %v", err)
	}
	if err := svc.RunRepairJob(context.Background(), job.ID); err != nil {
		t.Fatalf("RunRepairJob() error = %v", err)
	}
	job, err = svc.Store.GetRepairJob(context.Background(), job.ID)
	if err != nil || job.Status != domain.RepairStatusCompleted || job.CheckedObjects != 1 || job.RepairedObjects != 1 || job.FailedObjects != 0 {
		t.Fatalf("repair job=%+v err=%v", job, err)
	}

	if err := svc.DeleteObject(context.Background(), "images", "photos/demo.txt"); err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "replica", "images", "photos", "demo.txt")); !os.IsNotExist(err) {
		t.Fatalf("replica file still exists or stat failed unexpectedly: %v", err)
	}
	replicas, err = svc.Store.ListObjectReplicas(context.Background(), "images", "photos/demo.txt")
	if err != nil {
		t.Fatalf("ListObjectReplicas(after delete) error = %v", err)
	}
	if len(replicas) != 0 {
		t.Fatalf("replicas after delete = %+v", replicas)
	}
}
