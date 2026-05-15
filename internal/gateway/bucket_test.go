package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestBucketLevelCompatibility(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()

	create := httptest.NewRequest(http.MethodPut, "/bunbucket", nil)
	addAuth(create)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, create)
	if res.Code != http.StatusOK {
		t.Fatalf("create bucket status = %d body=%s", res.Code, res.Body.String())
	}

	head := httptest.NewRequest(http.MethodHead, "/bunbucket", nil)
	addAuth(head)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, head)
	if res.Code != http.StatusOK {
		t.Fatalf("head bucket status = %d body=%s", res.Code, res.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/", nil)
	addAuth(list)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, list)
	if res.Code != http.StatusOK {
		t.Fatalf("list buckets status = %d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "<Name>bunbucket</Name>") {
		t.Fatalf("list buckets missing bunbucket: %s", res.Body.String())
	}
}

func TestBucketLocationCompatibility(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/images?location", nil)
	addAuth(req)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("bucket location status = %d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "<LocationConstraint") || !strings.Contains(res.Body.String(), "auto") {
		t.Fatalf("unexpected location response: %s", res.Body.String())
	}
}

func newGatewayTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "local-test",
			Name:          "Local test",
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
	return NewHandler(svc), func() { _ = svc.Close() }
}
