package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/gateway"
	"github.com/gnurub/bucketmux/internal/store"
)

func TestAdminIndexShowsProviderForm(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{"Storage overview", "Action center", `id="admin-sidebar"`, `id="sidebar-toggle"`, `id="dashboard-search"`, `id="theme-toggle"`, `data-search-section`, `data-open-dialog="provider-dialog"`, `<dialog id="provider-dialog"`, `id="provider-catalog"`, `id="provider-catalog-filter"`, `hx-get="/admin/partials/provider-catalog"`, `htmx.org@2.0.10`, `data-provider-preset="aws"`, `data-provider-preset="cloudflare"`, `data-provider-preset="idrive"`, `data-provider-preset="azure"`, `data-provider-preset="oci"`, `data-provider-preset="hetzner"`, `data-provider-preset="scaleway"`, `data-provider-preset="ovh"`, `data-provider-preset="akamai"`, `data-provider-preset="custom"`, `id="provider-form"`, "cdn.jsdelivr.net/gh/glincker/thesvg@7870bc1c5f657d9accbb7f96cc457b8dd3363ee8", `<dialog id="bucket-dialog"`, `<dialog id="upload-dialog"`, `<dialog id="hook-dialog"`, `<dialog id="migration-dialog"`, `<dialog id="delete-object-dialog"`, "Delete permanently", "Migrate permanently", "Object browser", "Migration jobs", "Audit log", `id="object-browser-bucket"`, `id="migration-form"`, `id="migration-job-rows"`, `data-browse-objects`, "scrollIntoView", "/admin/api/objects/presign", "/admin/api/migrations", "Add / update provider", `name="secret_key"`, `value="s3-compatible"`, `settings_path`, `settings_cost_per_gb_month`, `settings_max_object_size_bytes`, `settings_monthly_upload_quota_bytes`, `id="admin-upload-form"`, `name="replication_provider_ids"`, "Provider health", "Provider quota", "Active alerts", "WASM processing pipelines", "WASM job history", "/admin/api/wasm-plugins", "Add / update HTTP hook", "Hook delivery history", "Secret headers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin body missing %q", want)
		}
	}
	for _, want := range []string{"Remote inventory", "Integrity and auto-repair", "Access credentials", "Recoverable trash", "Cost optimizer", `<dialog id="inventory-dialog"`, `<dialog id="repair-dialog"`, `<dialog id="credential-dialog"`, `<dialog id="placement-dialog"`, "Test connection", "Discover buckets", "Object Lock", "Run lifecycle", "/admin/api/inventory-jobs", "/admin/api/repair-jobs", "/admin/api/access-credentials", "/admin/api/placement-plan"} {
		if !strings.Contains(body, want) {
			t.Fatalf("advanced admin body missing %q", want)
		}
	}
}

func TestWASMPluginAdminAPIRejectsInvalidModuleAndListsSafely(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	body := strings.NewReader(`{"id":"bad","name":"Bad plugin","module_base64":"bm90IHdhc20=","enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/wasm-plugins/validate", body)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "not a WebAssembly binary") {
		t.Fatalf("validate status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/wasm-plugins", nil)
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "module_base64") {
		t.Fatalf("list status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/wasm-plugin-jobs", nil)
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("jobs status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestWASMPluginAdminAPIInstallsRustGuest(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_WASM_EXAMPLES") == "" {
		t.Skip("set BUCKETMUX_RUN_WASM_EXAMPLES=1 after building examples")
	}
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()
	module, err := os.ReadFile(filepath.Join("..", "..", "examples", "wasm", "rust", "target", "wasm32-wasip1", "release", "image-classifier.wasm"))
	if err != nil {
		t.Fatalf("read Rust guest: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"id": "rust-classifier", "name": "Rust classifier", "module_base64": base64.StdEncoding.EncodeToString(module),
		"events": []string{domain.WASMPluginEventObjectCreated}, "bucket_pattern": "images", "content_types": []string{"image/*"}, "enabled": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/wasm-plugins", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusCreated || strings.Contains(res.Body.String(), "module_base64") || !strings.Contains(res.Body.String(), "module_sha256") {
		t.Fatalf("install status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/admin/api/wasm-plugins/rust-classifier", nil)
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestEmbeddingAdminAPIListsWithoutValuesAndSearches(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()
	ctx := context.Background()
	object := domain.ObjectRecord{Bucket: "images", Key: "faces/alice.jpg", ProviderAccountID: "local-test", RemoteBucket: "images", RemoteKey: "faces/alice.jpg", Size: 3, ChecksumSHA256: "alice"}
	if err := handler.svc.Store.PutObject(ctx, object); err != nil {
		t.Fatal(err)
	}
	object, err := handler.svc.Store.GetObject(ctx, object.Bucket, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.svc.Store.ReplaceObjectEmbeddings(ctx, object, "face-plugin", []domain.WASMPluginEmbedding{{
		Kind: "face", Model: "arcface", ModelVersion: "1", Metric: "cosine", Dimensions: 3,
		Values: []float32{1, 0, 0}, Metadata: map[string]string{"box": "0,0,10,10"},
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/embeddings?bucket=images&key=faces%2Falice.jpg", nil)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), `"values"`) || !strings.Contains(res.Body.String(), `"model":"arcface"`) {
		t.Fatalf("list status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/embeddings/search", strings.NewReader(`{"bucket":"images","kind":"face","model":"arcface","model_version":"1","values":[1,0,0],"limit":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), `"values"`) || !strings.Contains(res.Body.String(), `"score":`) || !strings.Contains(res.Body.String(), "alice.jpg") {
		t.Fatalf("search status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/embeddings/capabilities", nil)
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"backend":"turso-native-exact"`) || !strings.Contains(res.Body.String(), `"engine":"turso"`) {
		t.Fatalf("capabilities status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestProviderCatalogHTMXFilter(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/admin/partials/provider-catalog?q=azure", nil)
	req.Header.Set("HX-Request", "true")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Microsoft Azure Blob Storage") || strings.Contains(res.Body.String(), "Hetzner Object Storage") {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestProviderBrandDetectionAndIconURLs(t *testing.T) {
	tests := []struct {
		name    string
		account domain.ProviderAccount
		brand   string
		hasIcon bool
	}{
		{name: "local", account: domain.ProviderAccount{Kind: domain.ProviderKindLocal}, brand: "local"},
		{name: "aws", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.us-east-1.amazonaws.com"}, brand: "aws", hasIcon: true},
		{name: "cloudflare", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://account.r2.cloudflarestorage.com"}, brand: "cloudflare", hasIcon: true},
		{name: "google cloud", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://storage.googleapis.com"}, brand: "gcs", hasIcon: true},
		{name: "backblaze", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.us-west-004.backblazeb2.com"}, brand: "backblaze", hasIcon: true},
		{name: "idrive", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.us-east-1.idrivee2.com"}, brand: "idrive"},
		{name: "azure", account: domain.ProviderAccount{Kind: domain.ProviderKindAzureBlob, Endpoint: "https://account.blob.core.windows.net"}, brand: "azure"},
		{name: "oci", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://namespace.compat.objectstorage.eu-frankfurt-1.oraclecloud.com"}, brand: "oci"},
		{name: "digitalocean", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://nyc3.digitaloceanspaces.com"}, brand: "digitalocean", hasIcon: true},
		{name: "hetzner", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://fsn1.your-objectstorage.com"}, brand: "hetzner"},
		{name: "scaleway", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.fr-par.scw.cloud"}, brand: "scaleway"},
		{name: "ovh", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.gra.io.cloud.ovh.net"}, brand: "ovh"},
		{name: "akamai", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://us-iad-18.linodeobjects.com"}, brand: "akamai"},
		{name: "wasabi", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://s3.us-east-1.wasabisys.com"}, brand: "wasabi", hasIcon: true},
		{name: "minio", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://minio.example.com"}, brand: "minio", hasIcon: true},
		{name: "cloudinary", account: domain.ProviderAccount{Kind: domain.ProviderKindCloudinary}, brand: "cloudinary", hasIcon: true},
		{name: "vercel", account: domain.ProviderAccount{Kind: domain.ProviderKindVercelBlob}, brand: "vercel", hasIcon: true},
		{name: "custom", account: domain.ProviderAccount{Kind: domain.ProviderKindS3Compat, Endpoint: "https://objects.example.com"}, brand: "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			brand := providerBrand(tt.account)
			if brand != tt.brand {
				t.Fatalf("providerBrand() = %q, want %q", brand, tt.brand)
			}
			if got := providerIconURL(brand); (got != "") != tt.hasIcon {
				t.Fatalf("providerIconURL(%q) = %q, hasIcon want %v", brand, got, tt.hasIcon)
			}
		})
	}
}

func TestAdminRejectsCrossSiteMutation(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/admin/buckets", strings.NewReader("name=blocked"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-site mutation status = %d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestAdminAcceptsConfiguredPublicOrigin(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()
	handler.svc.Config.Server.PublicBaseURL = "https://storage.example.com"

	req := httptest.NewRequest(http.MethodPost, "/admin/buckets", strings.NewReader("name=safe-origin"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://storage.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code == http.StatusForbidden {
		t.Fatalf("configured same-origin mutation was rejected: %s", res.Body.String())
	}
}

func TestAdminRejectsOversizedMutation(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()
	handler.svc.Config.Server.MaxAdminBodyBytes = 8

	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", strings.NewReader(`{"payload":"too large"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized mutation status = %d, want 413; body=%s", res.Code, res.Body.String())
	}
}

func TestAdminCreateProviderFromForm(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	form := url.Values{}
	form.Set("id", "local-form")
	form.Set("name", "Local form")
	form.Set("kind", string(domain.ProviderKindLocal))
	form.Set("bucket", "images")
	form.Set("capacity_bytes", "1000")
	form.Set("priority", "10")
	form.Set("settings_path", "./data/form-provider")
	form.Set("enabled", "on")

	req := httptest.NewRequest(http.MethodPost, "/admin/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestAdminCredentialLifecycleAndOpenAPI(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()
	createBody := `{"name":"Assets reader","role":"read-only","bucket_patterns":["images"],"prefix_patterns":["public/*"],"enabled":true}`
	request := httptest.NewRequest(http.MethodPost, "/admin/api/access-credentials", strings.NewReader(createBody))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("admin", "change-me")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create credential status=%d body=%s", response.Code, response.Body.String())
	}
	var created app.CreatedAccessCredential
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.SecretKey == "" || created.Credential.AccessKey == "" {
		t.Fatalf("created credential=%+v err=%v", created, err)
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/access-credentials", nil)
	request.SetBasicAuth("admin", "change-me")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), created.SecretKey) || !strings.Contains(response.Body.String(), created.Credential.AccessKey) {
		t.Fatalf("list credentials status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/openapi.json", nil)
	request.SetBasicAuth("admin", "change-me")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"openapi":"3.1.0"`) || !strings.Contains(response.Body.String(), "/admin/api/inventory-jobs") || !strings.Contains(response.Body.String(), "/admin/api/repair-jobs") || !strings.Contains(response.Body.String(), "/admin/api/provider-quotas") || !strings.Contains(response.Body.String(), "/admin/api/alerts") {
		t.Fatalf("OpenAPI status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminCreatesDurableRepairJob(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	request := httptest.NewRequest(http.MethodPost, "/admin/api/repair-jobs", strings.NewReader(`{"bucket":"images","prefix":"photos/"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("admin", "change-me")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"bucket":"images"`) {
		t.Fatalf("create repair status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/repair-jobs", nil)
	request.SetBasicAuth("admin", "change-me")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"prefix":"photos/"`) {
		t.Fatalf("list repair status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminCreateHookFromForm(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	form := url.Values{}
	form.Set("id", "notify-form")
	form.Set("name", "Notify form")
	form.Set("url", "https://example.com/hooks/bucketmux")
	form.Set("method", "POST")
	form.Add("events", domain.HookEventObjectCreated)
	form.Add("events", domain.HookEventObjectDeleted)
	form.Set("headers", "X-Webhook-Secret: super-secret\nAuthorization: Bearer test-token")
	form.Set("enabled", "on")

	req := httptest.NewRequest(http.MethodPost, "/admin/hooks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	hook, err := handler.svc.Store.GetHook(req.Context(), "notify-form")
	if err != nil {
		t.Fatalf("GetHook() error = %v", err)
	}
	if hook.URL != "https://example.com/hooks/bucketmux" || hook.Method != "POST" || !hook.Enabled || len(hook.Events) != 2 || hook.HeadersEncrypted == "" {
		t.Fatalf("hook = %+v", hook)
	}
	adminHooks, err := handler.svc.ListHooksForAdmin(req.Context())
	if err != nil {
		t.Fatalf("ListHooksForAdmin() error = %v", err)
	}
	if len(adminHooks) != 1 || strings.Join(adminHooks[0].HeaderNames, ",") != "Authorization,X-Webhook-Secret" || adminHooks[0].HeadersEncrypted != "" {
		t.Fatalf("admin hooks = %+v", adminHooks)
	}
}

func TestAdminCreateBucketWithReplicationProvidersFromForm(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "replicated-images")
	form.Add("replication_provider_ids", "local-test")

	req := httptest.NewRequest(http.MethodPost, "/admin/buckets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	bucket, err := handler.svc.Store.GetBucket(req.Context(), "replicated-images")
	if err != nil {
		t.Fatalf("GetBucket() error = %v", err)
	}
	if !bucket.ReplicationEnabled || len(bucket.ReplicationProviderIDs) != 1 || bucket.ReplicationProviderIDs[0] != "local-test" {
		t.Fatalf("bucket = %+v", bucket)
	}
}

func TestAdminCreateHookFromJSON(t *testing.T) {
	handler, cleanup := newTestAdminHandler(t)
	defer cleanup()

	body := strings.NewReader(`{"id":"notify-json","name":"Notify JSON","kind":"http","url":"https://example.com/hooks","method":"PATCH","events":["object.created"],"headers":{"X-Webhook-Secret":"json-secret"},"enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/hooks", body)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	hook, err := handler.svc.Store.GetHook(req.Context(), "notify-json")
	if err != nil {
		t.Fatalf("GetHook() error = %v", err)
	}
	if hook.Method != "PATCH" || hook.Kind != domain.HookKindHTTP || hook.HeadersEncrypted == "" {
		t.Fatalf("hook = %+v", hook)
	}
}

func TestAdminProviderHealthAPI(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/admin/api/provider-health", nil)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	var health []domain.ProviderHealth
	if err := json.NewDecoder(res.Body).Decode(&health); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(health) != 1 || health[0].ProviderAccountID != "local-test" || health[0].Status != domain.ProviderHealthHealthy {
		t.Fatalf("health = %+v", health)
	}
}

func TestAdminProviderQuotaAndAlertsAPI(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	request := httptest.NewRequest(http.MethodPost, "/admin/api/providers/local-test/quota/reconcile", nil)
	request.SetBasicAuth("admin", "change-me")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"provider_account_id":"local-test"`) || !strings.Contains(response.Body.String(), `"source":"remote-inventory"`) {
		t.Fatalf("reconcile status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/provider-quotas", nil)
	request.SetBasicAuth("admin", "change-me")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"provider_account_id":"local-test"`) || !strings.Contains(response.Body.String(), `"reliable":true`) {
		t.Fatalf("quotas status=%d body=%s", response.Code, response.Body.String())
	}

	if err := handler.svc.Store.UpsertAlert(t.Context(), domain.Alert{ID: "alert-test", DedupeKey: "provider:local-test", Type: domain.AlertTypeProviderDegraded, Severity: domain.AlertSeverityWarning, ProviderAccountID: "local-test", Message: "test degraded provider"}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/admin/api/alerts?status=open", nil)
	request.SetBasicAuth("admin", "change-me")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"provider.degraded"`) || !strings.Contains(response.Body.String(), "test degraded provider") {
		t.Fatalf("alerts status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminCreateMigrationJobFromJSON(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	err := handler.svc.Store.UpsertProvider(context.Background(), domain.ProviderAccount{
		ID:            "local-target",
		Name:          "Local target",
		Kind:          domain.ProviderKindLocal,
		Bucket:        "images",
		CapacityBytes: 1024 * 1024,
		Priority:      100,
		Enabled:       true,
		Settings:      map[string]string{"path": filepath.Join(t.TempDir(), "target")},
	})
	if err != nil {
		t.Fatalf("UpsertProvider(target) error = %v", err)
	}

	body := strings.NewReader(`{"bucket":"images","prefix":"photos/","source_provider_id":"local-test","target_provider_id":"local-target","mode":"copy"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/migrations", body)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	var job domain.MigrationJob
	if err := json.NewDecoder(res.Body).Decode(&job); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if job.Bucket != "images" || job.Prefix != "photos" || job.SourceProviderID != "local-test" || job.TargetProviderID != "local-target" {
		t.Fatalf("job = %+v", job)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/migrations?limit=5", nil)
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", res.Code, res.Body.String())
	}
	var jobs []domain.MigrationJob
	if err := json.NewDecoder(res.Body).Decode(&jobs); err != nil {
		t.Fatalf("Decode(list) error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("jobs = %+v, want job ID %s", jobs, job.ID)
	}
}

func TestAdminBrowseObjectsAndGeneratePublicURL(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	const objectBody = "public object through bucketmux"
	_, err := handler.svc.PutObject(context.Background(), domain.PutObjectInput{
		Bucket:      "images",
		Key:         "folder/cat.txt",
		Size:        int64(len(objectBody)),
		ContentType: "text/plain",
	}, strings.NewReader(objectBody))
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	_, err = handler.svc.PutObject(context.Background(), domain.PutObjectInput{
		Bucket:      "images",
		Key:         "folder/nested/dog.txt",
		Size:        int64(len("nested")),
		ContentType: "text/plain",
	}, strings.NewReader("nested"))
	if err != nil {
		t.Fatalf("PutObject(nested) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/objects?bucket=images&prefix=folder/", nil)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", res.Code, res.Body.String())
	}
	var list objectListResponse
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("Decode(list) error = %v", err)
	}
	if len(list.Objects) != 1 || list.Objects[0].Key != "folder/cat.txt" || len(list.Prefixes) != 1 || list.Prefixes[0] != "folder/nested/" {
		t.Fatalf("list = %+v", list)
	}

	presignPath := "/admin/api/objects/presign?bucket=images&key=" + url.QueryEscape("folder/cat.txt") + "&expires=600"
	req = httptest.NewRequest(http.MethodGet, presignPath, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "files.example.test")
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("presign status = %d body=%s", res.Code, res.Body.String())
	}
	var presigned objectPresignResponse
	if err := json.NewDecoder(res.Body).Decode(&presigned); err != nil {
		t.Fatalf("Decode(presign) error = %v", err)
	}
	if presigned.Method != http.MethodGet || presigned.ExpiresIn != 600 || !strings.HasPrefix(presigned.URL, "https://files.example.test/images/folder/cat.txt?") {
		t.Fatalf("presigned = %+v", presigned)
	}

	publicReq := httptest.NewRequest(http.MethodGet, presigned.URL, nil)
	publicRes := httptest.NewRecorder()
	gateway.NewHandler(handler.svc).ServeHTTP(publicRes, publicReq)

	if publicRes.Code != http.StatusOK {
		t.Fatalf("public GET status = %d body=%s", publicRes.Code, publicRes.Body.String())
	}
	if got := publicRes.Body.String(); got != objectBody {
		t.Fatalf("public body = %q, want %q", got, objectBody)
	}
}

func TestAdminPresignUsesConfiguredPublicBaseURL(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()
	handler.svc.Config.Server.PublicBaseURL = "https://cdn.example.test/bucketmux"

	_, err := handler.svc.PutObject(context.Background(), domain.PutObjectInput{
		Bucket:      "images",
		Key:         "folder/cat.txt",
		Size:        int64(len("cat")),
		ContentType: "text/plain",
	}, strings.NewReader("cat"))
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	path := "/admin/api/objects/presign?bucket=images&key=" + url.QueryEscape("folder/cat.txt") + "&expires=600"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("presign status = %d body=%s", res.Code, res.Body.String())
	}
	var presigned objectPresignResponse
	if err := json.NewDecoder(res.Body).Decode(&presigned); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !strings.HasPrefix(presigned.URL, "https://cdn.example.test/bucketmux/images/folder/cat.txt?") {
		t.Fatalf("presigned URL = %s", presigned.URL)
	}
}

func TestAdminDeleteObjectRequiresExactConfirmation(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	const objectBody = "delete me carefully"
	_, err := handler.svc.PutObject(context.Background(), domain.PutObjectInput{
		Bucket:      "images",
		Key:         "delete/me.txt",
		Size:        int64(len(objectBody)),
		ContentType: "text/plain",
	}, strings.NewReader(objectBody))
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	wrongBody := strings.NewReader(`{"bucket":"images","key":"delete/me.txt","confirm":"delete"}`)
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/objects", wrongBody)
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation status = %d body=%s", res.Code, res.Body.String())
	}
	if _, err := handler.svc.Store.GetObject(context.Background(), "images", "delete/me.txt"); err != nil {
		t.Fatalf("object should still exist after wrong confirmation: %v", err)
	}

	rightBody := strings.NewReader(`{"bucket":"images","key":"delete/me.txt","confirm":"Delete permanently"}`)
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/objects", rightBody)
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", res.Code, res.Body.String())
	}
	if _, err := handler.svc.Store.GetObject(context.Background(), "images", "delete/me.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetObject() error = %v, want ErrNotFound", err)
	}
	events, err := handler.svc.Store.ListAuditEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].Action != domain.AuditActionObjectDeleted || events[0].Bucket != "images" || events[0].Key != "delete/me.txt" {
		t.Fatalf("audit events = %+v", events)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{name: "zero", bytes: 0, want: "0 B"},
		{name: "bytes", bytes: 512, want: "512 B"},
		{name: "one kb", bytes: 1024, want: "1 KB"},
		{name: "fractional kb", bytes: 1536, want: "1.50 KB"},
		{name: "one mb", bytes: 1024 * 1024, want: "1 MB"},
		{name: "less than gb uses mb", bytes: 512 * 1024 * 1024, want: "512 MB"},
		{name: "one gb", bytes: 1024 * 1024 * 1024, want: "1 GB"},
		{name: "less than tb uses gb", bytes: 512 * 1024 * 1024 * 1024, want: "512 GB"},
		{name: "one tb", bytes: 1024 * 1024 * 1024 * 1024, want: "1 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatBytes(tt.bytes); got != tt.want {
				t.Fatalf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func newTestAdminHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:   config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{Name: "images"}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return NewHandler(svc), func() { _ = svc.Close() }
}

func TestAdminUploadObjectFromForm(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("bucket", "images")
	_ = writer.WriteField("key", "admin/upload.txt")
	part, err := writer.CreateFormFile("file", "upload.txt")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	_, _ = part.Write([]byte("uploaded from admin"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestAdminUploadObjectFromFormJSON(t *testing.T) {
	handler, cleanup := newTestAdminHandlerWithProvider(t)
	defer cleanup()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("bucket", "images")
	_ = writer.WriteField("key", "admin/ajax-upload.txt")
	part, err := writer.CreateFormFile("file", "upload.txt")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	_, _ = part.Write([]byte("uploaded from admin without refresh"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("admin", "change-me")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if contentType := res.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}

	var payload struct {
		OK                bool   `json:"ok"`
		Bucket            string `json:"bucket"`
		Key               string `json:"key"`
		Size              int64  `json:"size"`
		ProviderAccountID string `json:"providerAccountId"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !payload.OK || payload.Bucket != "images" || payload.Key != "admin/ajax-upload.txt" || payload.Size != int64(len("uploaded from admin without refresh")) || payload.ProviderAccountID != "local-test" {
		t.Fatalf("payload = %+v", payload)
	}
}

func newTestAdminHandlerWithProvider(t *testing.T) (*Handler, func()) {
	t.Helper()
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Admin:   config.AdminConfig{Enabled: true, Username: "admin", Password: "change-me"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "local-test",
			Name:          "Local test",
			Kind:          string(domain.ProviderKindLocal),
			Bucket:        "images",
			CapacityBytes: 1024 * 1024,
			Priority:      1,
			Enabled:       new(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return NewHandler(svc), func() { _ = svc.Close() }
}
