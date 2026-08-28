package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
)

type failoverAdapter struct {
	failure error
	health  string
	body    string
}

func (adapter *failoverAdapter) Put(_ context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	data, _ := io.ReadAll(body)
	if adapter.failure != nil {
		return domain.StoredObject{}, adapter.failure
	}
	adapter.body = string(data)
	return domain.StoredObject{ProviderAccountID: account.ID, RemoteBucket: account.Bucket, RemoteKey: input.StorageKey(), Size: int64(len(data)), ContentType: input.ContentType, ETag: `"ok"`, ChecksumSHA256: input.ChecksumSHA256}, nil
}
func (adapter *failoverAdapter) Get(context.Context, domain.ProviderAccount, domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	return nil, domain.ObjectRecord{}, fmt.Errorf("unused")
}
func (adapter *failoverAdapter) Head(context.Context, domain.ProviderAccount, domain.ObjectRecord) (domain.ObjectRecord, error) {
	return domain.ObjectRecord{}, fmt.Errorf("unused")
}
func (adapter *failoverAdapter) Delete(context.Context, domain.ProviderAccount, domain.ObjectRecord) error {
	return nil
}
func (adapter *failoverAdapter) Health(_ context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	status := adapter.health
	if status == "" {
		status = domain.ProviderHealthHealthy
	}
	return domain.ProviderHealth{ProviderAccountID: account.ID, Status: status, Message: status, CheckedAt: time.Now().UTC()}
}

func TestPutObjectFailsOverWithAtomicReservations(t *testing.T) {
	dataDir := t.TempDir()
	firstKind := domain.ProviderKind("test-throttled")
	secondKind := domain.ProviderKind("test-success")
	svc, err := NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "failover.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{
			{ID: "first", Name: "First", Kind: string(firstKind), Bucket: "images", CapacityBytes: 100, Priority: 1, Enabled: new(true)},
			{ID: "second", Name: "Second", Kind: string(secondKind), Bucket: "images", CapacityBytes: 100, Priority: 2, Enabled: new(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	failing := &failoverAdapter{failure: &provider.Error{Op: "put", Kind: provider.FailureThrottled, RetryAfter: time.Minute, Err: fmt.Errorf("rate limited")}}
	success := &failoverAdapter{}
	svc.Providers = provider.NewRegistry(provider.Entry(firstKind, failing), provider.Entry(secondKind, success))

	content := "the exact body must survive failover"
	object, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "failover.txt", Size: int64(len(content)), ContentType: "text/plain"}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if object.ProviderAccountID != "second" || success.body != content || object.ChecksumSHA256 == "" {
		t.Fatalf("object=%+v replayed=%q", object, success.body)
	}
	first, _ := svc.Store.GetProvider(context.Background(), "first")
	second, _ := svc.Store.GetProvider(context.Background(), "second")
	if first.AvailabilityStatus != string(provider.FailureThrottled) || first.ReservedBytes != 0 {
		t.Fatalf("first=%+v", first)
	}
	if second.UsedBytes != int64(len(content)) || second.ReservedBytes != 0 {
		t.Fatalf("second=%+v", second)
	}
	alerts, err := svc.Store.ListAlerts(context.Background(), domain.AlertStatusOpen, 10)
	if err != nil || len(alerts) != 1 || alerts[0].ProviderAccountID != "first" || alerts[0].Type != domain.AlertTypeProviderDegraded {
		t.Fatalf("alerts=%+v err=%v", alerts, err)
	}
}

func TestQuotaAlertEscalatesAndResolves(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "quota-alert.db"), MasterKey: "test-master-key"}, S3: config.S3Config{AccessKey: "ak", SecretKey: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	account := domain.ProviderAccount{ID: "quota-alert", Name: "Quota alert", Kind: domain.ProviderKindLocal, Bucket: "images", CapacityBytes: 100, UsedBytes: 86, Enabled: true, Settings: map[string]string{"quota_alert_threshold_percent": "85"}}
	if err := svc.Store.UpsertProvider(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	svc.updateQuotaAlert(t.Context(), account, quotaForAccount(account, time.Now().UTC()))
	alerts, err := svc.Store.ListAlerts(t.Context(), domain.AlertStatusOpen, 10)
	if err != nil || len(alerts) != 1 || alerts[0].Type != domain.AlertTypeQuotaNearLimit || alerts[0].Severity != domain.AlertSeverityWarning {
		t.Fatalf("near-limit alerts=%+v err=%v", alerts, err)
	}
	account.UsedBytes = 100
	svc.updateQuotaAlert(t.Context(), account, quotaForAccount(account, time.Now().UTC()))
	alerts, err = svc.Store.ListAlerts(t.Context(), domain.AlertStatusOpen, 10)
	if err != nil || len(alerts) != 1 || alerts[0].Type != domain.AlertTypeQuotaExhausted || alerts[0].Severity != domain.AlertSeverityCritical {
		t.Fatalf("exhausted alerts=%+v err=%v", alerts, err)
	}
	account.UsedBytes = 10
	svc.updateQuotaAlert(t.Context(), account, quotaForAccount(account, time.Now().UTC()))
	alerts, err = svc.Store.ListAlerts(t.Context(), domain.AlertStatusOpen, 10)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("resolved alerts=%+v err=%v", alerts, err)
	}
}

func TestEnabledProviderMustPassOnboarding(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "onboarding.db"), MasterKey: "test-master-key"}, S3: config.S3Config{AccessKey: "ak", SecretKey: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	kind := domain.ProviderKind("test-unhealthy")
	svc.Providers = provider.NewRegistry(provider.Entry(kind, &failoverAdapter{health: domain.ProviderHealthUnhealthy}))
	account := domain.ProviderAccount{ID: "bad", Name: "Bad", Kind: kind, Bucket: "images", Enabled: true}
	if err := svc.UpsertProviderFromAdmin(context.Background(), account, ""); err == nil || !strings.Contains(err.Error(), "onboarding") {
		t.Fatalf("error=%v", err)
	}
	account.Enabled = false
	if err := svc.UpsertProviderFromAdmin(context.Background(), account, ""); err != nil {
		t.Fatalf("disabled draft should be saved: %v", err)
	}
	account.Settings = map[string]string{"monthly_upload_quota_bytes": "not-a-number"}
	if err := svc.UpsertProviderFromAdmin(context.Background(), account, ""); err == nil || !strings.Contains(err.Error(), "non-negative integer") {
		t.Fatalf("invalid limits should fail onboarding: %v", err)
	}
}
