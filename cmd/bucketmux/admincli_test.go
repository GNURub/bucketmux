package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminCLIGetAndDeclarativeApply(t *testing.T) {
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		user, password, _ := r.BasicAuth()
		if user != "admin" || password != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	t.Setenv("BUCKETMUX_ADMIN_URL", server.URL)
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "secret")

	var output bytes.Buffer
	if err := runAdminCLI(t.Context(), []string{"providers"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"ok": true`) {
		t.Fatalf("output=%q", output.String())
	}
	if request := <-requests; request.Method != http.MethodGet || request.URL.Path != "/admin/api/providers" {
		t.Fatalf("GET request=%s %s", request.Method, request.URL.Path)
	}

	path := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(path, []byte("buckets:\n  - name: archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runAdminCLI(t.Context(), []string{"apply", path}, &output); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Method != http.MethodPost || request.URL.Path != "/admin/api/declarative/apply" || request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("apply request=%s %s headers=%v", request.Method, request.URL.Path, request.Header)
	}
}
