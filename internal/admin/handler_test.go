package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, want := range []string{"Action center", `data-open-dialog="provider-dialog"`, `<dialog id="provider-dialog"`, `<dialog id="bucket-dialog"`, `<dialog id="upload-dialog"`, `<dialog id="hook-dialog"`, `<dialog id="migration-dialog"`, `<dialog id="delete-object-dialog"`, "Eliminar permanentemente", "Migrar permanentemente", "Object browser", "Migration jobs", `id="object-browser-bucket"`, `id="migration-form"`, `id="migration-job-rows"`, `data-browse-objects`, "scrollIntoView", "/admin/api/objects/presign", "/admin/api/migrations", "Add / update provider", `name="secret_key"`, `value="s3-compatible"`, `settings_path`, `id="admin-upload-form"`, `name="replication_provider_ids"`, "Provider health", "Add / update HTTP hook", "Hook delivery history", "Secret headers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin body missing %q", want)
		}
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

	rightBody := strings.NewReader(`{"bucket":"images","key":"delete/me.txt","confirm":"Eliminar permanentemente"}`)
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
			Enabled:       boolPtr(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return NewHandler(svc), func() { _ = svc.Close() }
}

func boolPtr(v bool) *bool { return &v }
