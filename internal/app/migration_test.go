package app

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestRunMigrationJobMovesBucketPrefixBetweenProviders(t *testing.T) {
	svc, cleanup := newMigrationTestService(t)
	defer cleanup()

	ctx := context.Background()
	mustPutMigrationObject(t, svc, "photos/a.txt", "alpha")
	mustPutMigrationObject(t, svc, "photos/b.txt", "bravo")
	mustPutMigrationObject(t, svc, "other/c.txt", "charlie")

	job, err := svc.CreateMigrationJob(ctx, CreateMigrationJobInput{
		Bucket:           "images",
		Prefix:           "photos/",
		SourceProviderID: "local-source",
		TargetProviderID: "local-target",
		Mode:             domain.MigrationModeMove,
		Confirm:          MigrationMoveConfirmationPhrase,
	})
	if err != nil {
		t.Fatalf("CreateMigrationJob() error = %v", err)
	}
	if err := svc.RunMigrationJob(ctx, job.ID); err != nil {
		t.Fatalf("RunMigrationJob() error = %v", err)
	}

	job, err = svc.Store.GetMigrationJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetMigrationJob() error = %v", err)
	}
	if job.Status != domain.MigrationStatusCompleted || job.TotalObjects != 2 || job.SucceededObjects != 2 || job.FailedObjects != 0 {
		t.Fatalf("job = %+v", job)
	}

	for _, key := range []string{"photos/a.txt", "photos/b.txt"} {
		obj, err := svc.Store.GetObject(ctx, "images", key)
		if err != nil {
			t.Fatalf("GetObject(%s) error = %v", key, err)
		}
		if obj.ProviderAccountID != "local-target" {
			t.Fatalf("%s provider = %s, want local-target", key, obj.ProviderAccountID)
		}
	}
	unchanged, err := svc.Store.GetObject(ctx, "images", "other/c.txt")
	if err != nil {
		t.Fatalf("GetObject(other/c.txt) error = %v", err)
	}
	if unchanged.ProviderAccountID != "local-source" {
		t.Fatalf("other/c.txt provider = %s, want local-source", unchanged.ProviderAccountID)
	}

	body, _, err := svc.GetObject(ctx, "images", "photos/a.txt")
	if err != nil {
		t.Fatalf("GetObject(content) error = %v", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "alpha" {
		t.Fatalf("migrated content = %q, want alpha", string(data))
	}
}

func TestRunMigrationJobCopiesBucketPrefixAsReplicas(t *testing.T) {
	svc, cleanup := newMigrationTestService(t)
	defer cleanup()

	ctx := context.Background()
	mustPutMigrationObject(t, svc, "copy/demo.txt", "replicate-me")

	job, err := svc.CreateMigrationJob(ctx, CreateMigrationJobInput{
		Bucket:           "images",
		Prefix:           "copy/",
		SourceProviderID: "local-source",
		TargetProviderID: "local-target",
		Mode:             domain.MigrationModeCopy,
	})
	if err != nil {
		t.Fatalf("CreateMigrationJob() error = %v", err)
	}
	if err := svc.RunMigrationJob(ctx, job.ID); err != nil {
		t.Fatalf("RunMigrationJob() error = %v", err)
	}

	primary, err := svc.Store.GetObject(ctx, "images", "copy/demo.txt")
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	if primary.ProviderAccountID != "local-source" {
		t.Fatalf("primary provider = %s, want local-source", primary.ProviderAccountID)
	}
	replicas, err := svc.Store.ListObjectReplicas(ctx, "images", "copy/demo.txt")
	if err != nil {
		t.Fatalf("ListObjectReplicas() error = %v", err)
	}
	if len(replicas) != 1 || replicas[0].ProviderAccountID != "local-target" || replicas[0].Status != "succeeded" {
		t.Fatalf("replicas = %+v", replicas)
	}
}

func TestCreateMoveMigrationRequiresExactConfirmation(t *testing.T) {
	svc, cleanup := newMigrationTestService(t)
	defer cleanup()

	_, err := svc.CreateMigrationJob(context.Background(), CreateMigrationJobInput{
		Bucket:           "images",
		SourceProviderID: "local-source",
		TargetProviderID: "local-target",
		Mode:             domain.MigrationModeMove,
		Confirm:          "wrong",
	})
	if err == nil {
		t.Fatal("CreateMigrationJob() error = nil, want confirmation error")
	}
}

func mustPutMigrationObject(t *testing.T, svc *Service, key, body string) {
	t.Helper()
	obj, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: key, Size: int64(len(body)), ContentType: "text/plain"}, strings.NewReader(body))
	if err != nil {
		t.Fatalf("PutObject(%s) error = %v", key, err)
	}
	if obj.ProviderAccountID != "local-source" {
		t.Fatalf("PutObject(%s) provider = %s, want local-source", key, obj.ProviderAccountID)
	}
}

func newMigrationTestService(t *testing.T) (*Service, func()) {
	t.Helper()
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:   config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{
			{
				ID:            "local-source",
				Name:          "Local source",
				Kind:          string(domain.ProviderKindLocal),
				Bucket:        "images",
				CapacityBytes: 1024 * 1024,
				Priority:      1,
				Enabled:       new(true),
				Settings:      map[string]string{"path": filepath.Join(dataDir, "source")},
			},
			{
				ID:            "local-target",
				Name:          "Local target",
				Kind:          string(domain.ProviderKindLocal),
				Bucket:        "images",
				CapacityBytes: 1024 * 1024,
				Priority:      100,
				Enabled:       new(true),
				Settings:      map[string]string{"path": filepath.Join(dataDir, "target")},
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
	return svc, func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}
