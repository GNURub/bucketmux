package app

import (
	"context"
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
				Enabled:       boolPtr(true),
				Settings:      map[string]string{"path": filepath.Join(dataDir, "primary")},
			},
			{
				ID:            "local-replica",
				Name:          "Local replica",
				Kind:          string(domain.ProviderKindLocal),
				Bucket:        "images",
				CapacityBytes: 1024 * 1024,
				Priority:      100,
				Enabled:       boolPtr(true),
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
	defer svc.Close()

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
