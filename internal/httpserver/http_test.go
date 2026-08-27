package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestSecurityHeadersAllowOnlyTheProviderIconCDNForRemoteImages(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))

	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src 'self' data: https://cdn.jsdelivr.net;") {
		t.Fatalf("CSP does not allow the pinned provider icon CDN: %q", policy)
	}
	if strings.Contains(policy, "img-src *") || strings.Contains(policy, "http:") {
		t.Fatalf("CSP allows an overly broad remote image source: %q", policy)
	}
}

func TestHealthAndReadinessEndpoints(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "bucketmux.db"), MasterKey: "test-master-key"},
		S3:     config.S3Config{AccessKey: "ak", SecretKey: "sk"},
		Providers: []config.ProviderConfig{{
			ID: "local", Kind: string(domain.ProviderKindLocal), Bucket: "images", Enabled: new(true),
			Settings: map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	handler := NewHTTPHandler(svc)

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d body=%s", ready.Code, ready.Body.String())
	}
	var report app.ReadinessReport
	if err := json.NewDecoder(ready.Body).Decode(&report); err != nil || report.Status != "ready" || report.Checks["store"] != "ok" {
		t.Fatalf("ready report = %+v decode=%v", report, err)
	}

	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	notReady := httptest.NewRecorder()
	handler.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness after store close = %d, want 503; body=%s", notReady.Code, notReady.Body.String())
	}
	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness after dependency failure = %d, want 200", live.Code)
	}
}
