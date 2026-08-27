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

func TestVersionPromotionRebuildsCurrentReplicas(t *testing.T) {
	dataDir := t.TempDir()
	enabled := true
	svc, err := NewService(context.Background(), config.Config{
		Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "store.db"), MasterKey: "version-replication-master"},
		S3:     config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Buckets: []config.BucketConfig{{
			Name:                   "images",
			ReplicationProviderIDs: []string{"replica"},
			VersioningEnabled:      &enabled,
		}},
		Providers: []config.ProviderConfig{
			{ID: "primary", Name: "Primary", Kind: string(domain.ProviderKindLocal), Bucket: "images", CapacityBytes: 1 << 20, Priority: 1, Enabled: new(true), Settings: map[string]string{"path": filepath.Join(dataDir, "primary")}},
			{ID: "replica", Name: "Replica", Kind: string(domain.ProviderKindLocal), Bucket: "images", CapacityBytes: 1 << 20, Priority: 100, Enabled: new(true), Settings: map[string]string{"path": filepath.Join(dataDir, "replica")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.cancelWorkers != nil {
		svc.cancelWorkers()
		svc.workerWG.Wait()
	}
	defer func() { _ = svc.Close() }()

	first, err := svc.PutObject(t.Context(), domain.PutObjectInput{Bucket: "images", Key: "versioned.txt", Size: 3}, strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunObjectReplication(t.Context(), "images", "versioned.txt", "replica"); err != nil {
		t.Fatal(err)
	}
	second, err := svc.PutObject(t.Context(), domain.PutObjectInput{Bucket: "images", Key: "versioned.txt", Size: 3}, strings.NewReader("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.VersionID == second.VersionID || first.RemoteKey == second.RemoteKey {
		t.Fatalf("versions share storage: first=%+v second=%+v", first, second)
	}
	if err := svc.RunObjectReplication(t.Context(), "images", "versioned.txt", "replica"); err != nil {
		t.Fatal(err)
	}
	deleted, err := svc.DeleteObjectWithOptions(t.Context(), "images", "versioned.txt", DeleteObjectOptions{})
	if err != nil || !deleted.DeleteMarker {
		t.Fatalf("delete=%+v err=%v", deleted, err)
	}
	if replicas, err := svc.Store.ListObjectReplicas(t.Context(), "images", "versioned.txt"); err != nil || len(replicas) != 0 {
		t.Fatalf("delete marker replicas=%+v err=%v", replicas, err)
	}
	if _, err := svc.DeleteObjectWithOptions(t.Context(), "images", "versioned.txt", DeleteObjectOptions{VersionID: deleted.VersionID}); err != nil {
		t.Fatal(err)
	}
	replicas, err := svc.Store.ListObjectReplicas(t.Context(), "images", "versioned.txt")
	if err != nil || len(replicas) != 1 || replicas[0].Status != "pending" {
		t.Fatalf("promoted replicas=%+v err=%v", replicas, err)
	}
	if err := svc.RunObjectReplication(t.Context(), "images", "versioned.txt", "replica"); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Store.GetObject(t.Context(), "images", "versioned.txt")
	if err == nil {
		err = svc.Store.HydrateObjectAttributes(t.Context(), &current)
	}
	if err != nil || current.VersionID != second.VersionID {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if err := os.Remove(filepath.Join(dataDir, "primary", "images", filepath.FromSlash(second.RemoteKey))); err != nil {
		t.Fatal(err)
	}
	body, served, err := svc.GetObject(t.Context(), "images", "versioned.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || string(data) != "two" || served.ProviderAccountID != "replica" {
		t.Fatalf("replica fallback body=%q served=%+v err=%v", data, served, readErr)
	}
}
