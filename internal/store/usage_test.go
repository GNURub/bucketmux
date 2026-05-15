package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestListProviderBucketUsage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()
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
