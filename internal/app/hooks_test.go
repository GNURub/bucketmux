package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestHTTPHooksFireForObjectEvents(t *testing.T) {
	received := make(chan HookPayload, 2)
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	svc.HookHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if event := r.Header.Get("X-BucketMux-Event"); event == "" {
			t.Error("missing X-BucketMux-Event header")
		}
		if secret := r.Header.Get("X-Webhook-Secret"); secret != "super-secret" {
			t.Errorf("X-Webhook-Secret = %q, want super-secret", secret)
		}
		var payload HookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("Decode() error = %v", err)
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		received <- payload
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	})}

	if err := svc.UpsertHookFromAdmin(context.Background(), domain.Hook{
		ID:      "notify-test",
		Name:    "Notify test",
		Kind:    domain.HookKindHTTP,
		URL:     "http://example.test/hooks",
		Method:  http.MethodPost,
		Events:  []string{domain.HookEventObjectCreated, domain.HookEventObjectDeleted},
		Headers: map[string]string{"X-Webhook-Secret": "super-secret"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertHookFromAdmin() error = %v", err)
	}

	obj, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "hooks/demo.txt", Size: int64(len("hello hooks")), ContentType: "text/plain"}, strings.NewReader("hello hooks"))
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	created := waitHookPayload(t, received)
	if created.Event != domain.HookEventObjectCreated || created.Bucket != obj.Bucket || created.Key != obj.Key || created.ProviderAccountID != "local-hook-test" || created.Size != int64(len("hello hooks")) {
		t.Fatalf("created payload = %+v", created)
	}
	waitHookDeliveryStatus(t, svc, domain.HookEventObjectCreated, domain.HookDeliveryStatusSucceeded, 1)

	if err := svc.DeleteObject(context.Background(), "images", "hooks/demo.txt"); err != nil {
		t.Fatalf("DeleteObject() error = %v", err)
	}
	deleted := waitHookPayload(t, received)
	if deleted.Event != domain.HookEventObjectDeleted || deleted.Bucket != "images" || deleted.Key != "hooks/demo.txt" {
		t.Fatalf("deleted payload = %+v", deleted)
	}
	waitHookDeliveryStatus(t, svc, domain.HookEventObjectDeleted, domain.HookDeliveryStatusSucceeded, 1)
}

func TestHTTPHooksRetryFailedDeliveries(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	svc.HookRetryDelay = func(int) time.Duration { return time.Millisecond }

	var attempts atomic.Int32
	svc.HookHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		status := http.StatusNoContent
		if attempt == 1 {
			status = http.StatusInternalServerError
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	})}

	if err := svc.UpsertHookFromAdmin(context.Background(), domain.Hook{
		ID:      "retry-test",
		Name:    "Retry test",
		Kind:    domain.HookKindHTTP,
		URL:     "http://example.test/retry",
		Method:  http.MethodPost,
		Events:  []string{domain.HookEventObjectCreated},
		Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertHookFromAdmin() error = %v", err)
	}

	if _, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "hooks/retry.txt", Size: int64(len("retry")), ContentType: "text/plain"}, strings.NewReader("retry")); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	delivery := waitHookDeliveryStatus(t, svc, domain.HookEventObjectCreated, domain.HookDeliveryStatusSucceeded, 2)
	if delivery.Attempts != 2 || attempts.Load() != 2 {
		t.Fatalf("delivery attempts = %d, transport attempts = %d", delivery.Attempts, attempts.Load())
	}
}

func waitHookPayload(t *testing.T, received <-chan HookPayload) HookPayload {
	t.Helper()
	select {
	case payload := <-received:
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for hook payload")
	}
	return HookPayload{}
}

func waitHookDeliveryStatus(t *testing.T, svc *Service, event, status string, minAttempts int) domain.HookDelivery {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		deliveries, err := svc.Store.ListHookDeliveries(context.Background(), 20)
		if err != nil {
			t.Fatalf("ListHookDeliveries() error = %v", err)
		}
		for _, delivery := range deliveries {
			if delivery.Event == event && delivery.Status == status && delivery.Attempts >= minAttempts {
				return delivery
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for delivery event=%s status=%s minAttempts=%d", event, status, minAttempts)
	return domain.HookDelivery{}
}

func newHookTestService(t *testing.T) (*Service, func()) {
	t.Helper()
	dataDir := t.TempDir()
	svc, err := NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:   config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "local-hook-test",
			Name:          "Local hook test",
			Kind:          string(domain.ProviderKindLocal),
			Bucket:        "images",
			CapacityBytes: 1024 * 1024,
			Priority:      1,
			Enabled:       boolPtr(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return svc, func() { _ = svc.Close() }
}

func boolPtr(v bool) *bool { return &v }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
