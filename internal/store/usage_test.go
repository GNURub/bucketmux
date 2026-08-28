package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestOpenConfigAppliesSQLiteConnectionLimitToTursoEngine(t *testing.T) {
	s, err := OpenConfig(config.StoreConfig{
		Kind: "sqlite",
		SQLite: config.SQLiteStoreConfig{
			Path:         filepath.Join(t.TempDir(), "pool.db"),
			MaxOpenConns: 6,
			MaxIdleConns: 4,
		},
	}, "")
	if err != nil {
		t.Fatalf("OpenConfig() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if got := s.db.Stats().MaxOpenConnections; got != 6 {
		t.Fatalf("MaxOpenConnections = %d, want 6", got)
	}
}

func TestOpenConfigSQLiteUsesTursoEngine(t *testing.T) {
	s, err := OpenConfig(config.StoreConfig{
		Kind: "sqlite",
		SQLite: config.SQLiteStoreConfig{
			Path: filepath.Join(t.TempDir(), "legacy.db"),
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.dialect != dialectTurso || s.vectorBackend != "turso-native" {
		t.Fatalf("sqlite backend opened dialect=%q vectorBackend=%q", s.dialect, s.vectorBackend)
	}
}

func TestTursoConnectionsEnforceForeignKeys(t *testing.T) {
	s, err := OpenTurso(filepath.Join(t.TempDir(), "foreign-keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	first, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	for index, connection := range []*sql.Conn{first, second} {
		var enabled int
		if err := connection.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
			t.Fatalf("Turso connection %d foreign_keys=%d err=%v", index, enabled, err)
		}
	}
}

func TestListProviderBucketUsage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	ctx := context.Background()
	providers := []domain.ProviderAccount{
		{ID: "p1", Name: "p1", Kind: domain.ProviderKindLocal, Bucket: "remote", Enabled: true},
		{ID: "p2", Name: "p2", Kind: domain.ProviderKindLocal, Bucket: "remote", Enabled: true},
	}
	for _, provider := range providers {
		if err := s.UpsertProvider(ctx, provider); err != nil {
			t.Fatalf("UpsertProvider() error = %v", err)
		}
	}
	objects := []domain.ObjectRecord{
		{Bucket: "images", Key: "a", ProviderAccountID: "p1", RemoteBucket: "remote", RemoteKey: "a", Size: 10},
		{Bucket: "images", Key: "b", ProviderAccountID: "p1", RemoteBucket: "remote", RemoteKey: "b", Size: 20},
		{Bucket: "videos", Key: "c", ProviderAccountID: "p2", RemoteBucket: "remote", RemoteKey: "c", Size: 30},
	}
	for _, object := range objects {
		if err := s.PutObject(ctx, object); err != nil {
			t.Fatalf("PutObject() error = %v", err)
		}
	}
	usage, err := s.ListProviderBucketUsage(ctx)
	if err != nil {
		t.Fatalf("ListProviderBucketUsage() error = %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage len = %d, want 2: %#v", len(usage), usage)
	}
	if usage[0].ProviderAccountID != "p1" || usage[0].Bucket != "images" || usage[0].ObjectCount != 2 || usage[0].Bytes != 30 {
		t.Fatalf("unexpected first usage: %#v", usage[0])
	}
	if usage[1].ProviderAccountID != "p2" || usage[1].Bucket != "videos" || usage[1].ObjectCount != 1 || usage[1].Bytes != 30 {
		t.Fatalf("unexpected second usage: %#v", usage[1])
	}
}

func TestGetObjectWithProvider(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "object-provider.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	ctx := context.Background()
	account := domain.ProviderAccount{
		ID: "joined-provider", Name: "Joined provider", Kind: domain.ProviderKindS3Compat,
		Endpoint: "https://storage.example.test", Region: "eu-west-1", Bucket: "remote-images",
		AccessKey: "access", SecretEncrypted: "encrypted", CapacityBytes: 4096, UsedBytes: 512,
		Priority: 7, Enabled: true, Settings: map[string]string{"path_style": "true"},
	}
	if err := s.UpsertProvider(ctx, account); err != nil {
		t.Fatalf("UpsertProvider() error = %v", err)
	}
	object := domain.ObjectRecord{
		Bucket: "images", Key: "joined/object.txt", ProviderAccountID: account.ID,
		RemoteBucket: account.Bucket, RemoteKey: "objects/object.txt", Size: 42,
		ContentType: "text/plain", ETag: `"etag"`, ChecksumSHA256: "checksum", ReplicaStatus: "none",
	}
	if err := s.PutObject(ctx, object); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	gotObject, gotAccount, err := s.GetObjectWithProvider(ctx, object.Bucket, object.Key)
	if err != nil {
		t.Fatalf("GetObjectWithProvider() error = %v", err)
	}
	if gotObject.Key != object.Key || gotObject.RemoteKey != object.RemoteKey || gotObject.ETag != object.ETag {
		t.Fatalf("object = %+v, want key=%q remote_key=%q etag=%q", gotObject, object.Key, object.RemoteKey, object.ETag)
	}
	if gotAccount.ID != account.ID || gotAccount.Kind != account.Kind || gotAccount.Endpoint != account.Endpoint || gotAccount.Settings["path_style"] != "true" {
		t.Fatalf("provider = %+v, want provider %+v", gotAccount, account)
	}
	if _, _, err := s.GetObjectWithProvider(ctx, object.Bucket, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetObjectWithProvider(missing) error = %v, want ErrNotFound", err)
	}
}
