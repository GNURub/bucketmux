package app

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
)

type durableReplicaAdapter struct {
	data         []byte
	corruptHead  bool
	omitChecksum bool
	deletes      int
}

func (adapter *durableReplicaAdapter) Put(_ context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return domain.StoredObject{}, err
	}
	adapter.data = data
	return domain.StoredObject{ProviderAccountID: account.ID, RemoteBucket: account.Bucket, RemoteKey: input.StorageKey(), Size: int64(len(data)), ETag: `"replica"`, ChecksumSHA256: input.ChecksumSHA256}, nil
}

func (adapter *durableReplicaAdapter) Get(_ context.Context, _ domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	return io.NopCloser(strings.NewReader(string(adapter.data))), obj, nil
}

func (adapter *durableReplicaAdapter) Head(_ context.Context, _ domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	obj.Size = int64(len(adapter.data))
	switch {
	case adapter.corruptHead:
		obj.ChecksumSHA256 = "corrupt"
	case adapter.omitChecksum:
		obj.ChecksumSHA256 = ""
	}
	return obj, nil
}

func (adapter *durableReplicaAdapter) Delete(context.Context, domain.ProviderAccount, domain.ObjectRecord) error {
	adapter.deletes++
	adapter.data = nil
	return nil
}

func (adapter *durableReplicaAdapter) Health(_ context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	return domain.ProviderHealth{ProviderAccountID: account.ID, Status: domain.ProviderHealthHealthy, CheckedAt: time.Now().UTC()}
}

func TestReplicationRetriesChecksumMismatchAndVerifiesDownloadedBytes(t *testing.T) {
	dataDir := t.TempDir()
	targetKind := domain.ProviderKind("test-durable-replica")
	enabled := true
	svc, err := NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "replication.db"), MasterKey: "replication-master"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk"},
		Buckets: []config.BucketConfig{{Name: "images", ReplicationEnabled: true, ReplicationProviderIDs: []string{"target"}}},
		Providers: []config.ProviderConfig{
			{ID: "primary", Name: "Primary", Kind: string(domain.ProviderKindLocal), Bucket: "images", CapacityBytes: 1 << 20, Priority: 1, Enabled: &enabled, Settings: map[string]string{"path": filepath.Join(dataDir, "primary")}},
			{ID: "target", Name: "Target", Kind: string(targetKind), Bucket: "images", CapacityBytes: 1 << 20, Priority: 100, Enabled: &enabled},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.cancelWorkers != nil {
		svc.cancelWorkers()
		svc.workerWG.Wait()
	}
	t.Cleanup(func() { _ = svc.Close() })
	target := &durableReplicaAdapter{corruptHead: true}
	svc.Providers = provider.NewRegistry(
		provider.Entry(domain.ProviderKindLocal, provider.NewLocalAdapter(dataDir)),
		provider.Entry(targetKind, target),
	)

	content := "replication must verify every byte"
	object, err := svc.PutObject(t.Context(), domain.PutObjectInput{Bucket: "images", Key: "durable.txt", Size: int64(len(content))}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := svc.Store.ClaimNextObjectReplica(t.Context())
	if err != nil || !ok || claimed.Attempts != 1 {
		t.Fatalf("first claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := svc.RunObjectReplication(t.Context(), claimed.Bucket, claimed.Key, claimed.ProviderAccountID); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("first replication error=%v", err)
	}
	replicas, err := svc.Store.ListObjectReplicas(t.Context(), "images", "durable.txt")
	if err != nil || len(replicas) != 1 || replicas[0].Status != replicaStatusPending || replicas[0].Attempts != 1 || !replicas[0].NextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf("after failure replicas=%+v err=%v", replicas, err)
	}
	if target.deletes != 1 {
		t.Fatalf("corrupt replica deletes=%d want 1", target.deletes)
	}
	target.corruptHead = false
	target.omitChecksum = true
	retry := replicas[0]
	retry.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	if err := svc.Store.UpsertObjectReplica(t.Context(), retry); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = svc.Store.ClaimNextObjectReplica(t.Context())
	if err != nil || !ok || claimed.Attempts != 2 {
		t.Fatalf("retry claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := svc.RunObjectReplication(t.Context(), claimed.Bucket, claimed.Key, claimed.ProviderAccountID); err != nil {
		t.Fatal(err)
	}
	replicas, err = svc.Store.ListObjectReplicas(t.Context(), "images", "durable.txt")
	if err != nil || len(replicas) != 1 || replicas[0].Status != replicaStatusSucceeded || replicas[0].Attempts != 2 || replicas[0].ChecksumSHA256 != object.ChecksumSHA256 {
		t.Fatalf("successful replicas=%+v err=%v object=%+v", replicas, err, object)
	}
	account, err := svc.Store.GetProvider(t.Context(), "target")
	if err != nil || account.UsedBytes != int64(len(content)) || account.ReservedBytes != 0 {
		t.Fatalf("target account=%+v err=%v", account, err)
	}
	current, err := svc.Store.GetObject(t.Context(), "images", "durable.txt")
	if err != nil || current.ReplicaStatus != "completed" {
		t.Fatalf("current=%+v err=%v", current, err)
	}
}

var _ provider.Adapter = (*durableReplicaAdapter)(nil)
