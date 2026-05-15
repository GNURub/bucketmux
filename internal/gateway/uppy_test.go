package gateway

import (
	"bytes"
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

func TestUppyDirectUploadFlow(t *testing.T) {
	mux, cleanup := newUppyTestMux(t)
	defer cleanup()

	paramsBody := `{"bucket":"images","key":"uppy/direct.txt","contentType":"text/plain","expiresIn":60}`
	req := httptest.NewRequest(http.MethodPost, "http://bucketmux.local/uppy/s3/params", strings.NewReader(paramsBody))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("params status = %d body=%s", res.Code, res.Body.String())
	}
	var params struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Fields  map[string]string `json:"fields"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.Method != http.MethodPut || params.URL == "" || params.Headers["content-type"] != "text/plain" {
		t.Fatalf("unexpected params: %#v", params)
	}

	preflight := httptest.NewRequest(http.MethodOptions, params.URL, nil)
	preflight.Header.Set("Origin", "http://app.local")
	preflight.Header.Set("Access-Control-Request-Method", "PUT")
	res = httptest.NewRecorder()
	mux.ServeHTTP(res, preflight)
	if res.Code != http.StatusNoContent || res.Header().Get("Access-Control-Allow-Origin") == "" || !strings.Contains(res.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatalf("bad CORS preflight: code=%d headers=%v", res.Code, res.Header())
	}

	upload := httptest.NewRequest(http.MethodPut, params.URL, strings.NewReader("hello uppy"))
	upload.Header.Set("Content-Type", "text/plain")
	res = httptest.NewRecorder()
	mux.ServeHTTP(res, upload)
	if res.Code != http.StatusOK || res.Header().Get("ETag") == "" {
		t.Fatalf("upload status = %d headers=%v body=%s", res.Code, res.Header(), res.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "http://bucketmux.local/images/uppy/direct.txt", nil)
	addAuth(get)
	res = httptest.NewRecorder()
	mux.ServeHTTP(res, get)
	if res.Code != http.StatusOK || res.Body.String() != "hello uppy" {
		t.Fatalf("get status = %d body=%q", res.Code, res.Body.String())
	}
}

func TestUppyMultipartFlow(t *testing.T) {
	mux, cleanup := newUppyTestMux(t)
	defer cleanup()

	createReq := httptest.NewRequest(http.MethodPost, "http://bucketmux.local/uppy/s3/multipart/create", strings.NewReader(`{"bucket":"images","key":"uppy/multipart.txt","contentType":"text/plain"}`))
	createReq.Header.Set("Content-Type", "application/json")
	addAuth(createReq)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, createReq)
	if res.Code != http.StatusOK {
		t.Fatalf("create multipart status = %d body=%s", res.Code, res.Body.String())
	}
	var created struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
		Bucket   string `json:"bucket"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	parts := []string{"hello ", "multipart"}
	completeParts := make([]map[string]any, 0, len(parts))
	for i, body := range parts {
		partNumber := i + 1
		signBody := map[string]any{"bucket": "images", "key": created.Key, "uploadId": created.UploadID, "partNumber": partNumber, "expiresIn": 60}
		signed := postJSON[struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}](t, mux, "http://bucketmux.local/uppy/s3/multipart/sign", signBody)
		put := httptest.NewRequest(http.MethodPut, signed.URL, strings.NewReader(body))
		res = httptest.NewRecorder()
		mux.ServeHTTP(res, put)
		if res.Code != http.StatusOK || res.Header().Get("ETag") == "" {
			t.Fatalf("part %d upload status = %d headers=%v body=%s", partNumber, res.Code, res.Header(), res.Body.String())
		}
		completeParts = append(completeParts, map[string]any{"PartNumber": partNumber, "ETag": res.Header().Get("ETag")})
	}

	listed := postJSON[[]map[string]any](t, mux, "http://bucketmux.local/uppy/s3/multipart/list", map[string]any{"uploadId": created.UploadID})
	if len(listed) != 2 {
		t.Fatalf("listed parts len = %d, want 2: %#v", len(listed), listed)
	}
	completed := postJSON[map[string]string](t, mux, "http://bucketmux.local/uppy/s3/multipart/complete", map[string]any{"bucket": "images", "key": created.Key, "uploadId": created.UploadID, "parts": completeParts})
	if completed["location"] == "" {
		t.Fatalf("missing completed location: %#v", completed)
	}

	get := httptest.NewRequest(http.MethodGet, "http://bucketmux.local/images/uppy/multipart.txt", nil)
	addAuth(get)
	res = httptest.NewRecorder()
	mux.ServeHTTP(res, get)
	if res.Code != http.StatusOK || res.Body.String() != "hello multipart" {
		t.Fatalf("get status = %d body=%q", res.Code, res.Body.String())
	}
}

func postJSON[T any](t *testing.T, mux http.Handler, rawURL string, payload any) T {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, rawURL, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s status = %d body=%s", rawURL, res.Code, res.Body.String())
	}
	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func newUppyTestMux(t *testing.T) (*http.ServeMux, func()) {
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
	mux := http.NewServeMux()
	mux.Handle("/uppy/s3/", NewUppyHandler(svc))
	mux.Handle("/", NewHandler(svc))
	return mux, func() { _ = svc.Close() }
}
