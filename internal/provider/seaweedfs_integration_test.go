package provider

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestSeaweedFSS3CompatibleProvider(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION") != "1" {
		t.Skip("set BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION=1 to run SeaweedFS integration test")
	}
	endpoint := envOrDefault("SEAWEEDFS_S3_ENDPOINT", "http://localhost:8333")
	bucket := envOrDefault("SEAWEEDFS_S3_BUCKET", "images")
	accessKey := envOrDefault("SEAWEEDFS_S3_ACCESS_KEY", "admin")
	secretKey := envOrDefault("SEAWEEDFS_S3_SECRET_KEY", "secret")
	region := envOrDefault("SEAWEEDFS_S3_REGION", "us-east-1")

	adapter := NewS3CompatAdapter()
	account := domain.ProviderAccount{ID: "seaweedfs-test", Kind: domain.ProviderKindS3Compat, Endpoint: endpoint, Region: region, Bucket: bucket, AccessKey: accessKey, SecretKey: secretKey, Enabled: true}
	input := domain.PutObjectInput{Bucket: bucket, Key: "bucketmux-seaweedfs-integration.txt", Size: int64(len("hello seaweedfs")), ContentType: "text/plain"}

	stored, err := adapter.Put(context.Background(), account, input, bytes.NewBufferString("hello seaweedfs"))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	obj := domain.ObjectRecord{Bucket: bucket, Key: input.Key, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ContentType: stored.ContentType, ETag: stored.ETag}
	defer func() { _ = adapter.Delete(context.Background(), account, obj) }()

	head, err := adapter.Head(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if head.Size != input.Size {
		t.Fatalf("Head size = %d, want %d", head.Size, input.Size)
	}

	body, _, err := adapter.Get(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "hello seaweedfs" {
		t.Fatalf("body = %q, want hello seaweedfs", got)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
