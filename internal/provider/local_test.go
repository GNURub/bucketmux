package provider

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestLocalAdapterUsesConfiguredProviderPath(t *testing.T) {
	baseDir := t.TempDir()
	customDir := filepath.Join(baseDir, "disk-a")
	adapter := NewLocalAdapter(baseDir)
	account := domain.ProviderAccount{
		ID:       "local-a",
		Bucket:   "images",
		Settings: map[string]string{"path": customDir},
	}

	stored, err := adapter.Put(context.Background(), account, domain.PutObjectInput{Bucket: "images", Key: "cats/one.jpg", ContentType: "image/jpeg"}, bytes.NewBufferString("image-bytes"))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if stored.RemoteKey != filepath.Join("cats", "one.jpg") {
		t.Fatalf("remote key = %q", stored.RemoteKey)
	}

	content, err := os.ReadFile(filepath.Join(customDir, "images", "cats", "one.jpg"))
	if err != nil {
		t.Fatalf("expected object in configured path: %v", err)
	}
	if string(content) != "image-bytes" {
		t.Fatalf("content = %q, want image-bytes", content)
	}
}

func TestLocalAdapterRejectsPathTraversal(t *testing.T) {
	adapter := NewLocalAdapter(t.TempDir())
	account := domain.ProviderAccount{ID: "local-a", Bucket: "images"}

	_, err := adapter.Put(context.Background(), account, domain.PutObjectInput{Bucket: "images", Key: "../escape.jpg"}, bytes.NewBufferString("bad"))
	if err == nil {
		t.Fatal("expected traversal key to be rejected")
	}
}

func TestLocalAdapterRoundTrip(t *testing.T) {
	adapter := NewLocalAdapter(t.TempDir())
	account := domain.ProviderAccount{ID: "local-a", Bucket: "images"}

	stored, err := adapter.Put(context.Background(), account, domain.PutObjectInput{Bucket: "images", Key: "demo.txt", ContentType: "text/plain"}, bytes.NewBufferString("hello"))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	obj := domain.ObjectRecord{Bucket: "images", Key: "demo.txt", RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ContentType: stored.ContentType}
	body, _, err := adapter.Get(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
}
