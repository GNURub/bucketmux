package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/gateway"
	"github.com/gnurub/bucketmux/internal/router"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/domain"
)

type Handler struct {
	svc *app.Service
}

const objectDeleteConfirmationPhrase = "Eliminar permanentemente"

func NewHandler(svc *app.Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="BucketMux admin"`)
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "Admin credentials are required")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	if path == "" || path == "/" {
		h.index(w, r, "")
		return
	}
	switch {
	case path == "/providers" && r.Method == http.MethodPost:
		h.createProviderFromForm(w, r)
	case strings.HasPrefix(path, "/providers/") && strings.HasSuffix(path, "/delete") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/providers/"), "/delete")
		h.deleteProviderFromForm(w, r, id)
	case path == "/hooks" && r.Method == http.MethodPost:
		h.createHookFromForm(w, r)
	case strings.HasPrefix(path, "/hooks/") && strings.HasSuffix(path, "/delete") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/hooks/"), "/delete")
		h.deleteHookFromForm(w, r, id)
	case path == "/buckets" && r.Method == http.MethodPost:
		h.createBucketFromForm(w, r)
	case path == "/upload" && r.Method == http.MethodPost:
		h.uploadObjectFromForm(w, r)
	case path == "/api/providers":
		h.providers(w, r)
	case strings.HasPrefix(path, "/api/providers/"):
		h.providerByID(w, r, strings.TrimPrefix(path, "/api/providers/"))
	case path == "/api/hooks":
		h.hooks(w, r)
	case strings.HasPrefix(path, "/api/hooks/"):
		h.hookByID(w, r, strings.TrimPrefix(path, "/api/hooks/"))
	case path == "/api/hook-deliveries":
		h.hookDeliveries(w, r)
	case path == "/api/buckets":
		h.buckets(w, r)
	case path == "/api/usage":
		h.usage(w, r)
	case path == "/api/provider-health":
		h.providerHealth(w, r)
	case path == "/api/objects":
		h.objects(w, r)
	case path == "/api/objects/presign":
		h.presignObjectURL(w, r)
	case path == "/api/migrations":
		h.migrations(w, r)
	default:
		writeProblem(w, http.StatusNotFound, "not-found", "Admin route not found")
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	return ok && user == h.svc.Config.Admin.Username && pass == h.svc.Config.Admin.Password
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request, message string) {
	providers, err := h.svc.Store.ListProviders(r.Context(), false)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	for i := range providers {
		providers[i].SecretEncrypted = ""
		providers[i].SecretKey = ""
	}
	buckets, err := h.svc.Store.ListBuckets(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	usage, err := h.svc.Store.ListProviderBucketUsage(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	hooks, err := h.svc.ListHooksForAdmin(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	deliveries, err := h.svc.Store.ListHookDeliveries(r.Context(), 25)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	health, err := h.svc.ListProviderHealth(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "provider-health-error", err.Error())
		return
	}
	migrations, err := h.svc.Store.ListMigrationJobs(r.Context(), 10)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	auditEvents, err := h.svc.Store.ListAuditEvents(r.Context(), 10)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTemplate.Execute(w, adminPageData{Providers: providers, Buckets: buckets, Usage: usage, Hooks: hooks, HookDeliveries: deliveries, ProviderHealth: health, MigrationJobs: migrations, AuditEvents: auditEvents, TotalBytes: totalUsageBytes(usage), Message: message})
}

func (h *Handler) uploadObjectFromForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		h.uploadFormError(w, r, http.StatusBadRequest, "invalid-upload-form", "Invalid upload form: "+err.Error())
		return
	}
	bucket := strings.TrimSpace(r.FormValue("bucket"))
	key := strings.Trim(strings.TrimSpace(r.FormValue("key")), "/")
	file, header, err := r.FormFile("file")
	if err != nil {
		h.uploadFormError(w, r, http.StatusBadRequest, "missing-file", "File is required: "+err.Error())
		return
	}
	defer file.Close()
	if bucket == "" {
		h.uploadFormError(w, r, http.StatusBadRequest, "missing-bucket", "Bucket is required")
		return
	}
	if key == "" {
		key = header.Filename
	}
	if key == "" {
		h.uploadFormError(w, r, http.StatusBadRequest, "missing-key", "Object key is required")
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	object, err := h.svc.PutObject(r.Context(), domain.PutObjectInput{Bucket: bucket, Key: key, Size: header.Size, ContentType: contentType}, file)
	if err != nil {
		if errors.Is(err, router.ErrNoProviderAvailable) {
			h.uploadFormError(w, r, http.StatusInsufficientStorage, "no-provider-capacity", "Upload failed: no provider has enough capacity")
			return
		}
		h.uploadFormError(w, r, http.StatusBadRequest, "upload-failed", "Upload failed: "+err.Error())
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                true,
			"bucket":            object.Bucket,
			"key":               object.Key,
			"size":              object.Size,
			"etag":              object.ETag,
			"providerAccountId": object.ProviderAccountID,
		})
		return
	}
	h.redirectHome(w, r)
}

func (h *Handler) uploadFormError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if wantsJSON(r) {
		writeProblem(w, status, code, message)
		return
	}
	h.index(w, r, message)
}

func (h *Handler) createProviderFromForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.index(w, r, "Invalid provider form: "+err.Error())
		return
	}
	capacity, _ := strconv.ParseInt(r.FormValue("capacity_bytes"), 10, 64)
	priority, _ := strconv.Atoi(defaultString(r.FormValue("priority"), "100"))
	settings := map[string]string{}
	if path := strings.TrimSpace(r.FormValue("settings_path")); path != "" {
		settings["path"] = path
	}
	if cloudName := strings.TrimSpace(r.FormValue("settings_cloud_name")); cloudName != "" {
		settings["cloud_name"] = cloudName
	}
	if access := strings.TrimSpace(r.FormValue("settings_vercel_access")); access != "" {
		settings["access"] = access
	}
	if storeID := strings.TrimSpace(r.FormValue("settings_vercel_store_id")); storeID != "" {
		settings["store_id"] = storeID
	}
	if cost := strings.TrimSpace(r.FormValue("settings_cost_per_gb_month")); cost != "" {
		settings["cost_per_gb_month"] = cost
	}
	if maxObjectSize := strings.TrimSpace(r.FormValue("settings_max_object_size_bytes")); maxObjectSize != "" {
		settings["max_object_size_bytes"] = maxObjectSize
	}
	if minFree := strings.TrimSpace(r.FormValue("settings_min_free_bytes")); minFree != "" {
		settings["min_free_bytes"] = minFree
	}
	account := domain.ProviderAccount{
		ID:            strings.TrimSpace(r.FormValue("id")),
		Name:          strings.TrimSpace(r.FormValue("name")),
		Kind:          domain.ProviderKind(strings.TrimSpace(r.FormValue("kind"))),
		Endpoint:      strings.TrimSpace(r.FormValue("endpoint")),
		Region:        strings.TrimSpace(r.FormValue("region")),
		Bucket:        strings.TrimSpace(r.FormValue("bucket")),
		AccessKey:     strings.TrimSpace(r.FormValue("access_key")),
		CapacityBytes: capacity,
		Priority:      priority,
		Enabled:       r.FormValue("enabled") == "on",
		Settings:      settings,
	}
	if account.ID == "" || account.Kind == "" || account.Bucket == "" {
		h.index(w, r, "Provider id, kind and bucket are required")
		return
	}
	if account.Name == "" {
		account.Name = account.ID
	}
	if err := h.svc.UpsertProviderFromAdmin(r.Context(), account, r.FormValue("secret_key")); err != nil {
		h.index(w, r, "Could not save provider: "+err.Error())
		return
	}
	h.redirectHome(w, r)
}

func (h *Handler) deleteProviderFromForm(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.svc.Store.DeleteProvider(r.Context(), id); err != nil {
		h.index(w, r, "Could not delete provider: "+err.Error())
		return
	}
	h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionProviderDeleted, TargetID: id})
	h.redirectHome(w, r)
}

func (h *Handler) createHookFromForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.index(w, r, "Invalid hook form: "+err.Error())
		return
	}
	hook := domain.Hook{
		ID:      strings.TrimSpace(r.FormValue("id")),
		Name:    strings.TrimSpace(r.FormValue("name")),
		Kind:    domain.HookKindHTTP,
		URL:     strings.TrimSpace(r.FormValue("url")),
		Method:  strings.TrimSpace(r.FormValue("method")),
		Events:  normalizeHookEvents(r.Form["events"]),
		Headers: parseHookHeaderLines(r.FormValue("headers")),
		Enabled: r.FormValue("enabled") == "on",
	}
	if err := h.svc.UpsertHookFromAdmin(r.Context(), hook); err != nil {
		h.index(w, r, "Could not save hook: "+err.Error())
		return
	}
	h.redirectHome(w, r)
}

func (h *Handler) deleteHookFromForm(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.svc.Store.DeleteHook(r.Context(), id); err != nil {
		h.index(w, r, "Could not delete hook: "+err.Error())
		return
	}
	h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionHookDeleted, TargetID: id})
	h.redirectHome(w, r)
}

func (h *Handler) createBucketFromForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.index(w, r, "Invalid bucket form: "+err.Error())
		return
	}
	replicationProviderIDs := normalizeProviderIDs(r.Form["replication_provider_ids"])
	bucket := domain.Bucket{Name: strings.TrimSpace(r.FormValue("name")), ReplicationEnabled: len(replicationProviderIDs) > 0, ReplicationProviderIDs: replicationProviderIDs}
	if bucket.Name == "" {
		h.index(w, r, "Bucket name is required")
		return
	}
	if err := h.svc.Store.UpsertBucket(r.Context(), bucket); err != nil {
		h.index(w, r, "Could not save bucket: "+err.Error())
		return
	}
	h.redirectHome(w, r)
}

func (h *Handler) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) providers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		providers, err := h.svc.Store.ListProviders(r.Context(), false)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		for i := range providers {
			providers[i].SecretEncrypted = ""
			providers[i].SecretKey = ""
		}
		writeJSON(w, http.StatusOK, providers)
	case http.MethodPost:
		var req providerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		account := req.toDomain()
		if err := h.svc.UpsertProviderFromAdmin(r.Context(), account, req.SecretKey); err != nil {
			writeProblem(w, http.StatusBadRequest, "provider-upsert-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": account.ID})
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) providerByID(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	if err := h.svc.Store.DeleteProvider(r.Context(), id); err != nil {
		writeProblem(w, http.StatusInternalServerError, "provider-delete-failed", err.Error())
		return
	}
	h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionProviderDeleted, TargetID: id})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) hooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		hooks, err := h.svc.ListHooksForAdmin(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, hooks)
	case http.MethodPost:
		var req hookRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		hook := req.toDomain()
		if err := h.svc.UpsertHookFromAdmin(r.Context(), hook); err != nil {
			writeProblem(w, http.StatusBadRequest, "hook-upsert-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": hook.ID})
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) hookByID(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	if err := h.svc.Store.DeleteHook(r.Context(), id); err != nil {
		writeProblem(w, http.StatusInternalServerError, "hook-delete-failed", err.Error())
		return
	}
	h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionHookDeleted, TargetID: id})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) hookDeliveries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	deliveries, err := h.svc.Store.ListHookDeliveries(r.Context(), limit)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, deliveries)
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	usage, err := h.svc.Store.ListProviderBucketUsage(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (h *Handler) providerHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	health, err := h.svc.ListProviderHealth(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "provider-health-error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (h *Handler) migrations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		jobs, err := h.svc.Store.ListMigrationJobs(r.Context(), limit)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		var req migrationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		job, err := h.svc.CreateMigrationJob(r.Context(), app.CreateMigrationJobInput{
			Bucket:           req.Bucket,
			Prefix:           req.Prefix,
			SourceProviderID: req.SourceProviderID,
			TargetProviderID: req.TargetProviderID,
			Mode:             req.Mode,
			Confirm:          req.Confirm,
		})
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "migration-create-failed", err.Error())
			return
		}
		if job.Mode == domain.MigrationModeMove {
			h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionMigrationMove, Bucket: job.Bucket, Key: job.Prefix, TargetID: job.TargetProviderID, Detail: job.ID})
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) objects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listAdminObjects(w, r)
	case http.MethodDelete:
		h.deleteAdminObject(w, r)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) listAdminObjects(w http.ResponseWriter, r *http.Request) {
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	prefix := strings.TrimLeft(r.URL.Query().Get("prefix"), "/")
	startAfter := strings.TrimLeft(r.URL.Query().Get("start_after"), "/")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if bucket == "" {
		writeProblem(w, http.StatusBadRequest, "missing-bucket", "bucket is required")
		return
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	objects, err := h.svc.ListObjectsAfter(r.Context(), bucket, prefix, startAfter, limit)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "object-list-failed", err.Error())
		return
	}
	response := objectListResponse{
		Bucket:     bucket,
		Prefix:     prefix,
		StartAfter: startAfter,
		Limit:      limit,
	}
	prefixes := map[string]struct{}{}
	for _, obj := range objects {
		rest := strings.TrimPrefix(obj.Key, prefix)
		if slash := strings.Index(rest, "/"); slash >= 0 {
			prefixes[prefix+rest[:slash+1]] = struct{}{}
			continue
		}
		response.Objects = append(response.Objects, objectResponseFromDomain(obj))
	}
	for prefix := range prefixes {
		response.Prefixes = append(response.Prefixes, prefix)
	}
	sort.Strings(response.Prefixes)
	if len(objects) == limit {
		response.NextStartAfter = objects[len(objects)-1].Key
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) deleteAdminObject(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeObjectDeleteRequest(w, r)
	if !ok {
		return
	}
	if req.Confirm != objectDeleteConfirmationPhrase {
		writeProblem(w, http.StatusBadRequest, "invalid-confirmation", `confirmation must exactly match "Eliminar permanentemente"`)
		return
	}
	if req.Bucket == "" || req.Key == "" {
		writeProblem(w, http.StatusBadRequest, "missing-object", "bucket and key are required")
		return
	}
	if err := h.svc.DeleteObject(r.Context(), req.Bucket, req.Key); err != nil {
		writeProblem(w, http.StatusBadRequest, "object-delete-failed", err.Error())
		return
	}
	h.svc.RecordAuditEvent(r.Context(), domain.AuditEvent{Actor: app.AuditActorFromRequest(r), Action: domain.AuditActionObjectDeleted, Bucket: req.Bucket, Key: req.Key})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bucket": req.Bucket, "key": req.Key})
}

func (h *Handler) presignObjectURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	key := strings.TrimLeft(r.URL.Query().Get("key"), "/")
	expiresSeconds, err := parsePresignExpires(r.URL.Query().Get("expires"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-expiration", err.Error())
		return
	}
	if bucket == "" || key == "" {
		writeProblem(w, http.StatusBadRequest, "missing-object", "bucket and key are required")
		return
	}
	obj, err := h.svc.Store.GetObject(r.Context(), bucket, key)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "object-not-found", "object not found")
		return
	}
	target, err := url.Parse(h.adminPublicBaseURL(r) + "/" + url.PathEscape(bucket) + "/" + escapeS3Key(key))
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "url-build-failed", err.Error())
		return
	}
	auth := gateway.NewAuthenticator(h.svc.Config.S3)
	signedURL, ok := auth.PresignURL(http.MethodGet, *target, time.Duration(expiresSeconds)*time.Second)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "presign-failed", "could not generate presigned URL")
		return
	}
	writeJSON(w, http.StatusOK, objectPresignResponse{
		URL:               signedURL,
		Method:            http.MethodGet,
		Bucket:            obj.Bucket,
		Key:               obj.Key,
		ExpiresIn:         expiresSeconds,
		ExpiresAt:         time.Now().UTC().Add(time.Duration(expiresSeconds) * time.Second).Format(time.RFC3339),
		ProviderAccountID: obj.ProviderAccountID,
		ContentType:       obj.ContentType,
		Size:              obj.Size,
	})
}

func (h *Handler) buckets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		buckets, err := h.svc.Store.ListBuckets(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, buckets)
	case http.MethodPost:
		var bucket domain.Bucket
		if err := json.NewDecoder(r.Body).Decode(&bucket); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		if bucket.Name == "" {
			writeProblem(w, http.StatusBadRequest, "invalid-bucket", "name is required")
			return
		}
		bucket.ReplicationProviderIDs = normalizeProviderIDs(bucket.ReplicationProviderIDs)
		bucket.ReplicationEnabled = len(bucket.ReplicationProviderIDs) > 0
		if err := h.svc.Store.UpsertBucket(r.Context(), bucket); err != nil {
			writeProblem(w, http.StatusInternalServerError, "bucket-upsert-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, bucket)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

type providerRequest struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Endpoint      string            `json:"endpoint"`
	Region        string            `json:"region"`
	Bucket        string            `json:"bucket"`
	AccessKey     string            `json:"access_key"`
	SecretKey     string            `json:"secret_key"`
	CapacityBytes int64             `json:"capacity_bytes"`
	UsedBytes     int64             `json:"used_bytes"`
	Priority      int               `json:"priority"`
	Enabled       bool              `json:"enabled"`
	Settings      map[string]string `json:"settings"`
}

func (r providerRequest) toDomain() domain.ProviderAccount {
	if r.Settings == nil {
		r.Settings = map[string]string{}
	}
	return domain.ProviderAccount{
		ID:            r.ID,
		Name:          r.Name,
		Kind:          domain.ProviderKind(r.Kind),
		Endpoint:      r.Endpoint,
		Region:        r.Region,
		Bucket:        r.Bucket,
		AccessKey:     r.AccessKey,
		CapacityBytes: r.CapacityBytes,
		UsedBytes:     r.UsedBytes,
		Priority:      r.Priority,
		Enabled:       r.Enabled,
		Settings:      r.Settings,
	}
}

type hookRequest struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Kind    string            `json:"kind"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Events  []string          `json:"events"`
	Headers map[string]string `json:"headers"`
	Enabled bool              `json:"enabled"`
}

func (r hookRequest) toDomain() domain.Hook {
	return domain.Hook{
		ID:      r.ID,
		Name:    r.Name,
		Kind:    domain.HookKind(defaultString(r.Kind, string(domain.HookKindHTTP))),
		URL:     r.URL,
		Method:  r.Method,
		Events:  normalizeHookEvents(r.Events),
		Headers: r.Headers,
		Enabled: r.Enabled,
	}
}

type migrationRequest struct {
	Bucket           string `json:"bucket"`
	Prefix           string `json:"prefix"`
	SourceProviderID string `json:"source_provider_id"`
	TargetProviderID string `json:"target_provider_id"`
	Mode             string `json:"mode"`
	Confirm          string `json:"confirm"`
}

type objectListResponse struct {
	Bucket         string           `json:"bucket"`
	Prefix         string           `json:"prefix"`
	StartAfter     string           `json:"startAfter,omitempty"`
	Limit          int              `json:"limit"`
	Prefixes       []string         `json:"prefixes"`
	Objects        []objectResponse `json:"objects"`
	NextStartAfter string           `json:"nextStartAfter,omitempty"`
}

type objectResponse struct {
	Bucket            string `json:"bucket"`
	Key               string `json:"key"`
	ProviderAccountID string `json:"providerAccountId"`
	RemoteBucket      string `json:"remoteBucket"`
	RemoteKey         string `json:"remoteKey"`
	Size              int64  `json:"size"`
	ContentType       string `json:"contentType"`
	ETag              string `json:"etag"`
	ReplicaStatus     string `json:"replicaStatus"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
}

type objectPresignResponse struct {
	URL               string `json:"url"`
	Method            string `json:"method"`
	Bucket            string `json:"bucket"`
	Key               string `json:"key"`
	ExpiresIn         int    `json:"expiresIn"`
	ExpiresAt         string `json:"expiresAt"`
	ProviderAccountID string `json:"providerAccountId"`
	ContentType       string `json:"contentType"`
	Size              int64  `json:"size"`
}

type objectDeleteRequest struct {
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	Confirm string `json:"confirm"`
}

func decodeObjectDeleteRequest(w http.ResponseWriter, r *http.Request) (objectDeleteRequest, bool) {
	req := objectDeleteRequest{
		Bucket:  strings.TrimSpace(r.URL.Query().Get("bucket")),
		Key:     strings.TrimLeft(r.URL.Query().Get("key"), "/"),
		Confirm: r.URL.Query().Get("confirm"),
	}
	if r.Body != nil && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body objectDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return objectDeleteRequest{}, false
		}
		if body.Bucket != "" {
			req.Bucket = strings.TrimSpace(body.Bucket)
		}
		if body.Key != "" {
			req.Key = strings.TrimLeft(body.Key, "/")
		}
		if body.Confirm != "" {
			req.Confirm = body.Confirm
		}
	}
	return req, true
}

func objectResponseFromDomain(obj domain.ObjectRecord) objectResponse {
	return objectResponse{
		Bucket:            obj.Bucket,
		Key:               obj.Key,
		ProviderAccountID: obj.ProviderAccountID,
		RemoteBucket:      obj.RemoteBucket,
		RemoteKey:         obj.RemoteKey,
		Size:              obj.Size,
		ContentType:       obj.ContentType,
		ETag:              obj.ETag,
		ReplicaStatus:     obj.ReplicaStatus,
		CreatedAt:         obj.CreatedAt.Format(time.RFC3339),
		UpdatedAt:         obj.UpdatedAt.Format(time.RFC3339),
	}
}

type adminPageData struct {
	Providers      []domain.ProviderAccount
	Buckets        []domain.Bucket
	Usage          []domain.ProviderBucketUsage
	Hooks          []domain.Hook
	HookDeliveries []domain.HookDelivery
	ProviderHealth []domain.ProviderHealth
	MigrationJobs  []domain.MigrationJob
	AuditEvents    []domain.AuditEvent
	TotalBytes     int64
	Message        string
}

func totalUsageBytes(rows []domain.ProviderBucketUsage) int64 {
	var total int64
	for _, row := range rows {
		total += row.Bytes
	}
	return total
}

func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	const unit = 1024
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(bytes)
	unitIndex := 0
	for value >= unit && unitIndex < len(units)-1 {
		value /= unit
		unitIndex++
	}
	if unitIndex == 0 {
		return fmt.Sprintf("%d B", bytes)
	}
	if value == float64(int64(value)) || value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[unitIndex])
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, units[unitIndex])
	}
	return fmt.Sprintf("%.2f %s", value, units[unitIndex])
}

func migrationProgressPercent(job domain.MigrationJob) int {
	if job.TotalObjects <= 0 {
		if job.Status == domain.MigrationStatusCompleted {
			return 100
		}
		return 0
	}
	pct := int(float64(job.ProcessedObjects) / float64(job.TotalObjects) * 100)
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

func parsePresignExpires(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 900, nil
	}
	expires, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("expires must be a number of seconds")
	}
	if expires < 1 || expires > 604800 {
		return 0, fmt.Errorf("expires must be between 1 and 604800 seconds")
	}
	return expires, nil
}

func adminPublicBaseURL(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}

func (h *Handler) adminPublicBaseURL(r *http.Request) string {
	configured := strings.TrimRight(strings.TrimSpace(h.svc.Config.Server.PublicBaseURL), "/")
	if configured != "" {
		return configured
	}
	return adminPublicBaseURL(r)
}

func escapeS3Key(key string) string {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://bucketmux.local/errors/" + code,
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func normalizeHookEvents(events []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, event := range events {
		event = strings.TrimSpace(event)
		if event == "" || seen[event] {
			continue
		}
		seen[event] = true
		out = append(out, event)
	}
	if len(out) == 0 {
		return []string{domain.HookEventObjectCreated}
	}
	return out
}

func normalizeProviderIDs(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || containsString(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func parseHookHeaderLines(raw string) map[string]string {
	headers := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name != "" && value != "" {
			headers[name] = value
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func joinStrings(values []string, separator string) string {
	return strings.Join(values, separator)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

var indexTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"formatBytes": formatBytes,
	"join":        joinStrings,
	"progressPct": migrationProgressPercent,
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>BucketMux admin</title>
  <style>
    :root{
      color-scheme:dark;
      --bg:#000;
      --panel:#0a0a0a;
      --panel-2:#111;
      --line:#262626;
      --line-soft:#1a1a1a;
      --text:#fafafa;
      --muted:#a1a1aa;
      --muted-2:#71717a;
      --accent:#fff;
      --accent-text:#000;
      --danger:#ef4444;
      --success:#22c55e;
      --warning:#f59e0b;
      --radius:14px;
      --shadow:0 0 0 1px rgba(255,255,255,.06),0 24px 80px rgba(0,0,0,.45);
    }
    *{box-sizing:border-box}
    html{background:var(--bg)}
    body{
      margin:0;
      font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
      background:
        radial-gradient(circle at 50% -20%,rgba(255,255,255,.14),transparent 32rem),
        radial-gradient(circle at 85% 8%,rgba(59,130,246,.10),transparent 24rem),
        #000;
      color:var(--text);
      min-height:100vh;
    }
    a{color:inherit}
    .shell{max-width:1200px;margin:0 auto;padding:32px 20px 64px}
    .topbar{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:34px}
    .brand{display:flex;align-items:center;gap:12px}
    .logo{width:34px;height:34px;border-radius:10px;background:#fff;color:#000;display:grid;place-items:center;font-weight:900;letter-spacing:-.08em;box-shadow:0 0 0 1px rgba(255,255,255,.2)}
    .brand h1{font-size:18px;line-height:1.1;margin:0;letter-spacing:-.03em}
    .brand p{margin:3px 0 0;color:var(--muted);font-size:13px}
    .status{display:flex;gap:10px;align-items:center;color:var(--muted);font-size:13px;border:1px solid var(--line);border-radius:999px;padding:8px 12px;background:rgba(10,10,10,.76);backdrop-filter:blur(14px)}
    .dot{width:8px;height:8px;border-radius:99px;background:var(--success);box-shadow:0 0 18px rgba(34,197,94,.8)}
    .hero{border:1px solid var(--line);border-radius:24px;padding:26px;background:linear-gradient(180deg,rgba(255,255,255,.055),rgba(255,255,255,.02));box-shadow:var(--shadow);margin-bottom:18px}
    .hero h2{font-size:34px;letter-spacing:-.06em;margin:0 0 8px}.hero p{max-width:760px;color:var(--muted);margin:0;font-size:15px;line-height:1.7}.hero-actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:22px}
    .stats{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;margin-top:22px}.stat{border:1px solid var(--line-soft);border-radius:16px;background:rgba(0,0,0,.35);padding:14px}.stat strong{display:block;font-size:22px;letter-spacing:-.04em}.stat span{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.08em}
    .grid{display:grid;grid-template-columns:minmax(0,1.1fr) minmax(360px,.9fr);gap:18px;align-items:start}.stack{display:grid;gap:18px}
    .card{border:1px solid var(--line);border-radius:var(--radius);background:rgba(10,10,10,.88);box-shadow:0 0 0 1px rgba(255,255,255,.02);overflow:hidden}.card-header{padding:18px 20px;border-bottom:1px solid var(--line-soft);display:flex;align-items:flex-start;justify-content:space-between;gap:12px}.card-header-actions{display:flex;flex-wrap:wrap;gap:8px;justify-content:flex-end}.card-title{margin:0;font-size:15px;letter-spacing:-.02em}.card-desc{margin:5px 0 0;color:var(--muted);font-size:13px;line-height:1.5}.card-body{padding:20px}
    .notice{border:1px solid rgba(245,158,11,.3);background:rgba(245,158,11,.08);color:#fde68a;padding:12px 14px;border-radius:12px;margin-bottom:18px;font-size:13px}
    .notice.success{border-color:rgba(34,197,94,.35);background:rgba(34,197,94,.10);color:#bbf7d0}.notice.error{border-color:rgba(239,68,68,.38);background:rgba(239,68,68,.10);color:#fecaca}
    form{margin:0}.form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}.form-grid.one{grid-template-columns:1fr}
    label{display:grid;gap:7px;font-size:12px;color:#d4d4d8;font-weight:520}.hint{color:var(--muted-2);font-weight:400}
    input,select,textarea{width:100%;border:1px solid var(--line);border-radius:10px;background:#050505;color:var(--text);padding:11px 12px;outline:none;transition:border-color .15s,box-shadow .15s,background .15s}input:focus,select:focus,textarea:focus{border-color:#fff;box-shadow:0 0 0 3px rgba(255,255,255,.12);background:#000}input::placeholder,textarea::placeholder{color:#52525b}textarea{min-height:92px;resize:vertical;font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:12px}
    .checkbox{display:flex;align-items:center;gap:9px;color:var(--muted);font-size:13px;margin-top:2px}.checkbox input{width:auto}
    .actions{display:flex;gap:10px;align-items:center;margin-top:18px}.btn{appearance:none;border:1px solid transparent;border-radius:10px;background:#fff;color:#000;padding:10px 14px;font-weight:700;font-size:13px;cursor:pointer;transition:transform .12s,background .12s,border-color .12s;text-decoration:none;display:inline-flex;align-items:center;justify-content:center;gap:7px}.btn:hover{transform:translateY(-1px);background:#e5e5e5}.btn:disabled{cursor:not-allowed;opacity:.58;transform:none}.btn.secondary{background:#0a0a0a;color:#fff;border-color:var(--line)}.btn.danger{background:rgba(239,68,68,.12);border-color:rgba(239,68,68,.35);color:#fecaca}.btn.danger:hover{background:rgba(239,68,68,.2)}.btn.compact{padding:8px 10px;font-size:12px}
    .table-wrap{overflow:auto}.table{width:100%;border-collapse:separate;border-spacing:0}.table th{font-size:11px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted-2);font-weight:700;text-align:left;padding:0 12px 10px}.table td{border-top:1px solid var(--line-soft);padding:14px 12px;vertical-align:top;font-size:13px}.table tr:hover td{background:rgba(255,255,255,.025)}.name{font-weight:700}.sub{display:block;color:var(--muted);margin-top:3px}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:12px;color:#d4d4d8}.pill{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--line);background:#050505;border-radius:999px;padding:4px 8px;color:#e4e4e7;font-size:12px}.pill:before{content:"";width:6px;height:6px;border-radius:50%;background:var(--muted-2)}.pill.enabled:before,.pill.healthy:before,.pill.completed:before{background:var(--success)}.pill.disabled:before,.pill.pending:before{background:var(--muted-2)}.pill.degraded:before,.pill.running:before{background:var(--warning)}.pill.unhealthy:before,.pill.failed:before{background:var(--danger)}
    .empty{border:1px dashed var(--line);border-radius:14px;padding:22px;text-align:center;color:var(--muted);background:rgba(255,255,255,.02)}
    .browser-toolbar{display:grid;grid-template-columns:minmax(160px,.8fr) minmax(220px,1fr) minmax(130px,.45fr);gap:12px;align-items:end}.prefix-list{display:flex;flex-wrap:wrap;gap:8px;margin:16px 0}.folder-btn{appearance:none;border:1px solid var(--line);border-radius:999px;background:#050505;color:#fff;padding:8px 11px;font-size:12px;cursor:pointer}.folder-btn:hover{background:#18181b}.public-url-panel{margin-top:16px}.public-url-panel textarea{min-height:74px}.row-actions{display:flex;gap:8px;flex-wrap:wrap}
    .progress{width:150px;max-width:100%;height:8px;border-radius:999px;background:#18181b;overflow:hidden;border:1px solid var(--line)}.progress span{display:block;height:100%;background:#fff;border-radius:999px}.progress-label{display:block;margin-top:6px;color:var(--muted);font-size:12px}
    .code{font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;background:#050505;border:1px solid var(--line);border-radius:12px;padding:12px;color:#d4d4d8;overflow:auto;font-size:12px;line-height:1.6}.footer{margin-top:24px;color:var(--muted-2);font-size:12px;text-align:center}
    .admin-dialog{width:min(720px,calc(100vw - 28px));max-height:calc(100vh - 40px);border:1px solid var(--line);border-radius:20px;background:linear-gradient(180deg,#111,#080808);color:var(--text);box-shadow:var(--shadow);padding:0;overflow:hidden}.admin-dialog::backdrop{background:rgba(0,0,0,.72);backdrop-filter:blur(8px)}.dialog-header{padding:18px 20px;border-bottom:1px solid var(--line-soft);display:flex;align-items:flex-start;justify-content:space-between;gap:14px}.dialog-body{padding:20px;overflow:auto;max-height:calc(100vh - 152px)}.dialog-close{appearance:none;border:1px solid var(--line);background:#050505;color:#fff;border-radius:10px;width:34px;height:34px;cursor:pointer;font-size:20px;line-height:1}.dialog-close:hover{background:#18181b}
    @media (max-width:900px){.grid,.stats,.browser-toolbar{grid-template-columns:1fr}.form-grid{grid-template-columns:1fr}.hero h2{font-size:28px}.topbar{align-items:flex-start;flex-direction:column}}
  </style>
</head>
<body>
  <main class="shell">
    <header class="topbar">
      <div class="brand"><div class="logo">B</div><div><h1>BucketMux</h1><p>Self-hosted S3-compatible storage gateway</p></div></div>
      <div class="status"><span class="dot"></span> Admin enabled</div>
    </header>

    <section class="hero">
      <h2>Storage routing, without vendor lock-in.</h2>
      <p>Configura proveedores gratuitos, cuentas locales o S3-compatible, y deja que el gateway enrute subidas desde una API compatible con S3.</p>
      <div class="hero-actions">
        <button class="btn" type="button" data-open-dialog="provider-dialog">New provider</button>
        <button class="btn secondary" type="button" data-open-dialog="bucket-dialog">New bucket</button>
        <button class="btn secondary" type="button" data-open-dialog="upload-dialog">Upload object</button>
        <button class="btn secondary" type="button" data-browse-objects>Browse objects</button>
        <button class="btn secondary" type="button" data-open-dialog="migration-dialog">Migrate</button>
        <button class="btn secondary" type="button" data-open-dialog="hook-dialog">New hook</button>
      </div>
      <div class="stats">
        <div class="stat"><strong>{{len .Providers}}</strong><span>Providers</span></div>
        <div class="stat"><strong>{{len .Buckets}}</strong><span>Logical buckets</span></div>
        <div class="stat"><strong>{{formatBytes .TotalBytes}}</strong><span>Indexed storage</span></div>
      </div>
    </section>

    {{if .Message}}<div class="notice">{{.Message}}</div>{{end}}

    <div class="grid">
      <div class="stack">
        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Providers</h2><p class="card-desc">Credenciales cifradas. Los secretos existentes no se muestran nunca.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="provider-dialog">New provider</button></div></div>
          <div class="card-body table-wrap">
            {{if .Providers}}
            <table class="table">
              <thead><tr><th>Provider</th><th>Type</th><th>Target</th><th>Usage</th><th>Status</th><th></th></tr></thead>
              <tbody>
              {{range .Providers}}
                <tr>
                  <td><span class="name">{{.ID}}</span><span class="sub">{{.Name}}</span></td>
                  <td><span class="pill">{{.Kind}}</span></td>
                  <td><span class="mono">{{if .Endpoint}}{{.Endpoint}}{{else}}{{index .Settings "path"}}{{end}}</span><span class="sub">bucket: {{.Bucket}}</span></td>
                  <td><span class="mono">{{formatBytes .UsedBytes}} / {{formatBytes .CapacityBytes}}</span><span class="sub">priority {{.Priority}}</span></td>
                  <td>{{if .Enabled}}<span class="pill enabled">enabled</span>{{else}}<span class="pill disabled">disabled</span>{{end}}</td>
                  <td><form method="post" action="/admin/providers/{{.ID}}/delete"><button class="btn danger" type="submit">Delete</button></form></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No providers yet. Add your first local, S3-compatible or Cloudinary provider.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Provider health</h2><p class="card-desc">Comprobación básica de configuración, credenciales y acceso al backend.</p></div></div>
          <div class="card-body table-wrap">
            {{if .ProviderHealth}}
            <table class="table">
              <thead><tr><th>Provider</th><th>Health</th><th>Message</th><th>Latency</th><th>Checked</th></tr></thead>
              <tbody>
              {{range .ProviderHealth}}
                <tr>
                  <td><span class="mono">{{.ProviderAccountID}}</span></td>
                  <td><span class="pill {{.Status}}">{{.Status}}</span></td>
                  <td>{{.Message}}</td>
                  <td><span class="mono">{{.LatencyMillis}} ms</span></td>
                  <td><span class="mono">{{.CheckedAt}}</span></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No providers configured yet.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Usage by provider and bucket</h2><p class="card-desc">Espacio ocupado según el índice local de objetos gestionados por BucketMux.</p></div></div>
          <div class="card-body table-wrap">
            {{if .Usage}}
            <table class="table"><thead><tr><th>Provider</th><th>Bucket</th><th>Objects</th><th>Size</th></tr></thead><tbody>{{range .Usage}}<tr><td><span class="mono">{{.ProviderAccountID}}</span></td><td>{{.Bucket}}</td><td>{{.ObjectCount}}</td><td><span class="mono">{{formatBytes .Bytes}}</span></td></tr>{{end}}</tbody></table>
            {{else}}<div class="empty">No indexed objects yet. Upload files to see usage per provider and bucket.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Hooks</h2><p class="card-desc">Llamadas HTTP salientes disparadas por eventos de objetos.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="hook-dialog">New hook</button></div></div>
          <div class="card-body table-wrap">
            {{if .Hooks}}
            <table class="table">
              <thead><tr><th>Hook</th><th>Request</th><th>Events</th><th>Secret headers</th><th>Status</th><th></th></tr></thead>
              <tbody>
              {{range .Hooks}}
                <tr>
                  <td><span class="name">{{.ID}}</span><span class="sub">{{.Name}}</span></td>
                  <td><span class="mono">{{.Method}} {{.URL}}</span><span class="sub">type: {{.Kind}}</span></td>
                  <td><span class="mono">{{join .Events ", "}}</span></td>
                  <td><span class="mono">{{if .HeaderNames}}{{join .HeaderNames ", "}}{{else}}—{{end}}</span></td>
                  <td>{{if .Enabled}}<span class="pill enabled">enabled</span>{{else}}<span class="pill disabled">disabled</span>{{end}}</td>
                  <td><form method="post" action="/admin/hooks/{{.ID}}/delete"><button class="btn danger" type="submit">Delete</button></form></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No hooks yet. Add an HTTP hook to receive object event notifications.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Hook delivery history</h2><p class="card-desc">Últimas entregas, intentos y errores. Los reintentos quedan pendientes hasta completarse o agotar intentos.</p></div></div>
          <div class="card-body table-wrap">
            {{if .HookDeliveries}}
            <table class="table">
              <thead><tr><th>Delivery</th><th>Hook</th><th>Object</th><th>Status</th><th>Attempts</th><th>Last result</th></tr></thead>
              <tbody>
              {{range .HookDeliveries}}
                <tr>
                  <td><span class="mono">{{.ID}}</span><span class="sub">{{.Event}}</span></td>
                  <td><span class="mono">{{.HookID}}</span></td>
                  <td><span class="mono">{{.Bucket}}/{{.Key}}</span></td>
                  <td><span class="pill">{{.Status}}</span><span class="sub">next: {{.NextAttemptAt}}</span></td>
                  <td><span class="mono">{{.Attempts}} / {{.MaxAttempts}}</span></td>
                  <td><span class="mono">{{.LastStatusCode}}</span><span class="sub">{{.LastError}}</span></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No hook deliveries yet. Upload or delete objects to generate hook events.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Buckets</h2><p class="card-desc">Buckets lógicos expuestos por el gateway.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="bucket-dialog">New bucket</button><button class="btn compact secondary" type="button" data-open-dialog="upload-dialog">Upload object</button></div></div>
          <div class="card-body">
            {{if .Buckets}}
            <div class="table-wrap"><table class="table"><thead><tr><th>Name</th><th>Replication targets</th></tr></thead><tbody>{{range .Buckets}}<tr><td>{{.Name}}</td><td><span class="mono">{{if .ReplicationProviderIDs}}{{join .ReplicationProviderIDs ", "}}{{else}}none{{end}}</span></td></tr>{{end}}</tbody></table></div>
            {{else}}<div class="empty">No buckets yet. Create a logical bucket to expose it through the S3 API.</div>
            {{end}}
          </div>
        </section>

        <section id="object-browser-card" class="card">
          <div class="card-header"><div><h2 class="card-title">Object browser</h2><p class="card-desc">Navega por los objetos indexados y genera URLs públicas firmadas para acceder a través de BucketMux.</p></div><div class="card-header-actions"><button class="btn compact secondary" type="button" data-open-dialog="upload-dialog">Upload object</button></div></div>
          <div class="card-body">
            {{if .Buckets}}
            <div class="browser-toolbar">
              <label>Bucket
                <select id="object-browser-bucket">
                  {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
                </select>
              </label>
              <label>Prefix <span class="hint">navega como carpetas usando /</span><input id="object-browser-prefix" placeholder="uploads/"></label>
              <label>Public URL expiry
                <select id="object-browser-expiry">
                  <option value="900">15 minutes</option>
                  <option value="3600">1 hour</option>
                  <option value="21600">6 hours</option>
                  <option value="86400">1 day</option>
                  <option value="604800">7 days</option>
                </select>
              </label>
            </div>
            <div class="actions"><button id="object-browser-load" class="btn" type="button">Load objects</button><button id="object-browser-up" class="btn secondary" type="button">Up one level</button></div>
            <div id="object-browser-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
            <div id="object-browser-prefixes" class="prefix-list"></div>
            <div class="table-wrap" style="margin-top:12px">
              <table class="table">
                <thead><tr><th>Object</th><th>Size</th><th>Provider</th><th>Updated</th><th>Actions</th></tr></thead>
                <tbody id="object-browser-rows"><tr><td colspan="5"><div class="empty">Load a bucket to browse objects.</div></td></tr></tbody>
              </table>
            </div>
            <div id="object-public-url-panel" class="public-url-panel" hidden>
              <label>Public presigned URL <span class="hint">cualquiera con esta URL puede leer el objeto hasta que expire</span><textarea id="object-public-url" readonly></textarea></label>
              <div class="actions"><a id="object-public-url-open" class="btn" href="#" target="_blank" rel="noopener">Open URL</a><button id="object-public-url-copy" class="btn secondary" type="button">Copy URL</button></div>
            </div>
            {{else}}<div class="empty">Create a bucket first, then upload objects to browse them.</div>{{end}}
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Migration jobs</h2><p class="card-desc">Historial y progreso de migraciones por bucket/prefix entre proveedores.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="migration-dialog">New migration</button></div></div>
          <div class="card-body table-wrap">
            <div id="migration-jobs-empty" class="empty" {{if .MigrationJobs}}hidden{{end}}>No migration jobs yet. Start one to move or copy a bucket/prefix between providers.</div>
            <table id="migration-jobs-table" class="table" {{if not .MigrationJobs}}hidden{{end}}>
              <thead><tr><th>Job</th><th>Scope</th><th>Route</th><th>Progress</th><th>Status</th><th>Last error</th></tr></thead>
              <tbody id="migration-job-rows">
              {{range .MigrationJobs}}
                <tr>
                  <td><span class="mono">{{.ID}}</span><span class="sub">{{.Mode}}</span></td>
                  <td><span class="mono">{{.Bucket}}/{{.Prefix}}</span></td>
                  <td><span class="mono">{{.SourceProviderID}} → {{.TargetProviderID}}</span></td>
                  <td><div class="progress"><span style="width:{{progressPct .}}%"></span></div><span class="progress-label">{{.ProcessedObjects}} / {{.TotalObjects}} · ok {{.SucceededObjects}} · failed {{.FailedObjects}}</span></td>
                  <td><span class="pill {{.Status}}">{{.Status}}</span><span class="sub">{{.CurrentKey}}</span></td>
                  <td><span class="sub">{{.LastError}}</span></td>
                </tr>
              {{end}}
              </tbody>
            </table>
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Audit log</h2><p class="card-desc">Operaciones destructivas y de movimiento iniciadas desde el admin.</p></div></div>
          <div class="card-body table-wrap">
            {{if .AuditEvents}}
            <table class="table">
              <thead><tr><th>Action</th><th>Target</th><th>Actor</th><th>Detail</th></tr></thead>
              <tbody>
              {{range .AuditEvents}}
                <tr>
                  <td><span class="pill failed">{{.Action}}</span><span class="sub">{{.CreatedAt.Format "2006-01-02 15:04:05 UTC"}}</span></td>
                  <td><span class="mono">{{.Bucket}}/{{.Key}}</span><span class="sub">{{.TargetID}}</span></td>
                  <td><span class="mono">{{.Actor}}</span></td>
                  <td><span class="sub">{{.Detail}}</span></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No destructive admin operations recorded yet.</div>{{end}}
          </div>
        </section>
      </div>

      <aside class="stack">
        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Action center</h2><p class="card-desc">Mantén el panel limpio: los formularios se abren bajo demanda.</p></div></div>
          <div class="card-body">
            <div class="form-grid one">
              <button class="btn" type="button" data-open-dialog="provider-dialog">Add / update provider</button>
              <button class="btn secondary" type="button" data-open-dialog="bucket-dialog">Create / update bucket</button>
              <button class="btn secondary" type="button" data-open-dialog="upload-dialog">Upload object</button>
              <button class="btn secondary" type="button" data-browse-objects>Browse objects</button>
              <button class="btn secondary" type="button" data-open-dialog="migration-dialog">Migrate bucket/prefix</button>
              <button class="btn secondary" type="button" data-open-dialog="hook-dialog">Add / update HTTP hook</button>
            </div>
          </div>
        </section>

        <section class="card">
          <div class="card-header"><div><h2 class="card-title">Quick test</h2><p class="card-desc">Con las credenciales locales por defecto.</p></div></div>
          <div class="card-body"><pre class="code">curl -X PUT http://localhost:8080/images/demo.txt \
  -H 'X-S3LS-Access-Key: local-access-key' \
  -H 'X-S3LS-Secret-Key: local-secret-key' \
  --data 'hola mundo'</pre></div>
        </section>
      </aside>
    </div>

    <dialog id="provider-dialog" class="admin-dialog" aria-labelledby="provider-dialog-title">
      <div class="dialog-header"><div><h2 id="provider-dialog-title" class="card-title">Add / update provider</h2><p class="card-desc">Usa el mismo ID para actualizar. Deja el secret vacío para conservar el actual.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form method="post" action="/admin/providers">
          <div class="form-grid one">
            <label>ID<input required name="id" placeholder="r2-main"></label>
            <label>Name<input name="name" placeholder="Cloudflare R2 main"></label>
            <label>Type<select name="kind" required><option value="local">local</option><option value="s3-compatible">s3-compatible</option><option value="cloudinary">cloudinary</option><option value="vercel-blob">vercel-blob</option></select></label>
            <label>Remote bucket / cloud name<input required name="bucket" placeholder="images"></label>
            <label>Endpoint <span class="hint">S3-compatible only</span><input name="endpoint" placeholder="https://ACCOUNT.r2.cloudflarestorage.com"></label>
            <label>Region<input name="region" value="auto"></label>
            <label>Access key / API key<input name="access_key" autocomplete="off"></label>
            <label>Secret key / API secret<input name="secret_key" type="password" autocomplete="new-password" placeholder="Keep empty to preserve"></label>
            <div class="form-grid">
              <label>Capacity bytes<input name="capacity_bytes" type="number" value="10737418240"></label>
              <label>Priority<input name="priority" type="number" value="100"></label>
            </div>
            <label>Local path <span class="hint">kind=local</span><input name="settings_path" placeholder="./data/local-provider"></label>
            <label>Cloudinary cloud_name<input name="settings_cloud_name" placeholder="my-cloud"></label>
            <label>Vercel Blob access <span class="hint">kind=vercel-blob; public or private</span><input name="settings_vercel_access" placeholder="public"></label>
            <label>Vercel Blob store_id <span class="hint">optional if token includes it</span><input name="settings_vercel_store_id" placeholder="store-id"></label>
            <label>Cost per GB/month <span class="hint">policy: lower cost wins on priority tie</span><input name="settings_cost_per_gb_month" placeholder="0.015"></label>
            <label>Max object size bytes <span class="hint">policy: skip provider for larger objects</span><input name="settings_max_object_size_bytes" type="number" placeholder="104857600"></label>
            <label>Min free bytes <span class="hint">policy: reserve capacity headroom</span><input name="settings_min_free_bytes" type="number" placeholder="1073741824"></label>
            <label class="checkbox"><input type="checkbox" name="enabled" checked> Enabled</label>
          </div>
          <div class="actions"><button class="btn" type="submit">Save provider</button><button class="btn secondary" type="reset">Reset</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="bucket-dialog" class="admin-dialog" aria-labelledby="bucket-dialog-title">
      <div class="dialog-header"><div><h2 id="bucket-dialog-title" class="card-title">Create / update bucket</h2><p class="card-desc">Configura buckets lógicos y sus proveedores de réplica.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form method="post" action="/admin/buckets">
          <div class="form-grid one">
            <label>Bucket name<input required name="name" placeholder="images"></label>
            <label>Replication providers <span class="hint">opcional; selecciona 0 o más destinos de réplica</span>
              <select name="replication_provider_ids" multiple size="5">
                {{range .Providers}}<option value="{{.ID}}">{{.ID}} — {{.Name}}</option>{{end}}
              </select>
            </label>
          </div>
          <div class="actions"><button class="btn" type="submit">Save bucket policy</button><button class="btn secondary" type="reset">Reset</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="upload-dialog" class="admin-dialog" aria-labelledby="upload-dialog-title">
      <div class="dialog-header"><div><h2 id="upload-dialog-title" class="card-title">Upload object</h2><p class="card-desc">Sube un archivo a un bucket lógico usando el routing normal de BucketMux.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="admin-upload-form" method="post" action="/admin/upload" enctype="multipart/form-data">
          <div class="form-grid one">
            <label>Bucket
              <select name="bucket" required>
                {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
              </select>
            </label>
            <label>Object key <span class="hint">opcional; usa el nombre del archivo si está vacío</span><input name="key" placeholder="uploads/photo.jpg"></label>
            <label>File<input required name="file" type="file"></label>
          </div>
          <div id="upload-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button class="btn" type="submit">Upload file</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="hook-dialog" class="admin-dialog" aria-labelledby="hook-dialog-title">
      <div class="dialog-header"><div><h2 id="hook-dialog-title" class="card-title">Add / update HTTP hook</h2><p class="card-desc">Se ejecuta después de que BucketMux confirme el cambio en su índice.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form method="post" action="/admin/hooks">
          <div class="form-grid one">
            <label>ID<input required name="id" placeholder="notify-api"></label>
            <label>Name<input name="name" placeholder="Notify API"></label>
            <label>URL<input required name="url" type="url" placeholder="https://example.com/bucketmux/hooks"></label>
            <label>Method<select name="method"><option value="POST">POST</option><option value="PUT">PUT</option><option value="PATCH">PATCH</option><option value="GET">GET</option></select></label>
            <label>Secret headers <span class="hint">uno por línea, formato Header-Name: value; se cifran y no se vuelven a mostrar</span><textarea name="headers" placeholder="X-Webhook-Secret: super-secret&#10;Authorization: Bearer token"></textarea></label>
            <label class="checkbox"><input type="checkbox" name="events" value="object.created" checked> object.created</label>
            <label class="checkbox"><input type="checkbox" name="events" value="object.deleted"> object.deleted</label>
            <label class="checkbox"><input type="checkbox" name="enabled" checked> Enabled</label>
          </div>
          <div class="actions"><button class="btn" type="submit">Save hook</button><button class="btn secondary" type="reset">Reset</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="migration-dialog" class="admin-dialog" aria-labelledby="migration-dialog-title">
      <div class="dialog-header"><div><h2 id="migration-dialog-title" class="card-title">Migrate bucket/prefix</h2><p class="card-desc">Crea un job en background para copiar o mover objetos entre proveedores.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="migration-form">
          <div class="form-grid one">
            <label>Bucket
              <select id="migration-bucket" required>
                {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
              </select>
            </label>
            <label>Prefix <span class="hint">opcional; vacío migra todo el bucket</span><input id="migration-prefix" placeholder="uploads/2026/"></label>
            <label>From provider
              <select id="migration-source-provider" required>
                {{range .Providers}}<option value="{{.ID}}">{{.ID}} — {{.Name}}</option>{{end}}
              </select>
            </label>
            <label>To provider
              <select id="migration-target-provider" required>
                {{range .Providers}}<option value="{{.ID}}">{{.ID}} — {{.Name}}</option>{{end}}
              </select>
            </label>
            <label>Mode
              <select id="migration-mode">
                <option value="copy">Copy — keep source as primary and add target replica</option>
                <option value="move">Move — make target primary and delete source best-effort</option>
              </select>
            </label>
            <label id="migration-confirm-label" hidden>Move confirmation <span class="hint">escribe exactamente: Migrar permanentemente</span><input id="migration-confirm" autocomplete="off" placeholder="Migrar permanentemente"></label>
          </div>
          <div id="migration-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button id="migration-submit" class="btn" type="submit">Start migration job</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="delete-object-dialog" class="admin-dialog" aria-labelledby="delete-object-dialog-title">
      <div class="dialog-header"><div><h2 id="delete-object-dialog-title" class="card-title">Delete object permanently</h2><p class="card-desc">Esta acción elimina el objeto del proveedor primario, sus réplicas conocidas y el índice local.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="delete-object-form">
          <div class="notice error">Acción destructiva. Para confirmar escribe exactamente: <strong>Eliminar permanentemente</strong></div>
          <div class="form-grid one">
            <label>Object<input id="delete-object-key" readonly></label>
            <label>Confirmation<input id="delete-object-confirmation" autocomplete="off" placeholder="Eliminar permanentemente"></label>
          </div>
          <div class="actions"><button id="delete-object-submit" class="btn danger" type="submit" disabled>Delete object</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <p class="footer">BucketMux · Embedded admin · JSON API available under /admin/api/* · usage: /admin/api/usage</p>
  </main>
  <script>
    (() => {
      const openButtons = document.querySelectorAll('[data-open-dialog]');
      const closeButtons = document.querySelectorAll('[data-dialog-close]');

      openButtons.forEach((button) => {
        button.addEventListener('click', () => {
          const dialog = document.getElementById(button.getAttribute('data-open-dialog'));
          if (!dialog) return;
          if (typeof dialog.showModal === 'function') {
            dialog.showModal();
          } else {
            dialog.setAttribute('open', '');
          }
          const firstField = dialog.querySelector('input,select,textarea,button[type="submit"]');
          if (firstField && typeof firstField.focus === 'function') firstField.focus();
        });
      });

      closeButtons.forEach((button) => {
        button.addEventListener('click', () => {
          const dialog = button.closest('dialog');
          if (!dialog) return;
          if (typeof dialog.close === 'function') {
            dialog.close();
          } else {
            dialog.removeAttribute('open');
          }
        });
      });

      document.querySelectorAll('dialog.admin-dialog').forEach((dialog) => {
        dialog.addEventListener('click', (event) => {
          if (event.target !== dialog) return;
          if (typeof dialog.close === 'function') dialog.close();
        });
      });

      const objectBucket = document.getElementById('object-browser-bucket');
      const objectPrefix = document.getElementById('object-browser-prefix');
      const objectExpiry = document.getElementById('object-browser-expiry');
      const objectLoad = document.getElementById('object-browser-load');
      const objectUp = document.getElementById('object-browser-up');
      const objectStatus = document.getElementById('object-browser-status');
      const objectPrefixes = document.getElementById('object-browser-prefixes');
      const objectRows = document.getElementById('object-browser-rows');
      const publicURLPanel = document.getElementById('object-public-url-panel');
      const publicURLField = document.getElementById('object-public-url');
      const publicURLOpen = document.getElementById('object-public-url-open');
      const publicURLCopy = document.getElementById('object-public-url-copy');
      const deleteObjectDialog = document.getElementById('delete-object-dialog');
      const deleteObjectForm = document.getElementById('delete-object-form');
      const deleteObjectKey = document.getElementById('delete-object-key');
      const deleteObjectConfirmation = document.getElementById('delete-object-confirmation');
      const deleteObjectSubmit = document.getElementById('delete-object-submit');
      let pendingDeleteKey = '';
      const deletePhrase = 'Eliminar permanentemente';

      const humanBytes = (bytes) => {
        const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
        let value = Number(bytes || 0);
        let index = 0;
        while (value >= 1024 && index < units.length - 1) {
          value = value / 1024;
          index++;
        }
        if (index === 0) return String(value) + ' B';
        if (value >= 100 || Number.isInteger(value)) return value.toFixed(0) + ' ' + units[index];
        if (value >= 10) return value.toFixed(1) + ' ' + units[index];
        return value.toFixed(2) + ' ' + units[index];
      };

      const showObjectStatus = (kind, message) => {
        if (!objectStatus) return;
        objectStatus.hidden = false;
        objectStatus.className = 'notice' + (kind ? ' ' + kind : '');
        objectStatus.textContent = message;
      };

      const hideObjectStatus = () => {
        if (objectStatus) objectStatus.hidden = true;
      };

      const setPublicURL = (value) => {
        if (!publicURLPanel || !publicURLField || !publicURLOpen) return;
        publicURLPanel.hidden = !value;
        publicURLField.value = value || '';
        publicURLOpen.href = value || '#';
      };

      const openDeleteObjectDialog = (key) => {
        pendingDeleteKey = key || '';
        if (deleteObjectKey) deleteObjectKey.value = pendingDeleteKey;
        if (deleteObjectConfirmation) deleteObjectConfirmation.value = '';
        if (deleteObjectSubmit) deleteObjectSubmit.disabled = true;
        if (!deleteObjectDialog) return;
        if (typeof deleteObjectDialog.showModal === 'function') {
          deleteObjectDialog.showModal();
        } else {
          deleteObjectDialog.setAttribute('open', '');
        }
        if (deleteObjectConfirmation && typeof deleteObjectConfirmation.focus === 'function') deleteObjectConfirmation.focus();
      };

      const parentPrefix = (prefix) => {
        const clean = String(prefix || '').replace(/^\/+|\/+$/g, '');
        if (!clean) return '';
        const parts = clean.split('/');
        parts.pop();
        return parts.length ? parts.join('/') + '/' : '';
      };

      const renderObjects = (payload) => {
        if (!objectPrefixes || !objectRows) return;
        objectPrefixes.textContent = '';
        objectRows.textContent = '';
        (payload.prefixes || []).forEach((prefix) => {
          const button = document.createElement('button');
          button.type = 'button';
          button.className = 'folder-btn';
          button.textContent = '📁 ' + prefix;
          button.addEventListener('click', () => {
            objectPrefix.value = prefix;
            loadObjects();
          });
          objectPrefixes.appendChild(button);
        });
        const objects = payload.objects || [];
        if (objects.length === 0 && (!payload.prefixes || payload.prefixes.length === 0)) {
          const row = document.createElement('tr');
          const cell = document.createElement('td');
          cell.colSpan = 5;
          const empty = document.createElement('div');
          empty.className = 'empty';
          empty.textContent = 'No objects found for this prefix.';
          cell.appendChild(empty);
          row.appendChild(cell);
          objectRows.appendChild(row);
          return;
        }
        objects.forEach((object) => {
          const row = document.createElement('tr');
          const nameCell = document.createElement('td');
          const name = document.createElement('span');
          name.className = 'name';
          name.textContent = object.key;
          const meta = document.createElement('span');
          meta.className = 'sub';
          meta.textContent = object.contentType || 'application/octet-stream';
          nameCell.appendChild(name);
          nameCell.appendChild(meta);

          const sizeCell = document.createElement('td');
          sizeCell.className = 'mono';
          sizeCell.textContent = humanBytes(object.size);

          const providerCell = document.createElement('td');
          const provider = document.createElement('span');
          provider.className = 'mono';
          provider.textContent = object.providerAccountId || '—';
          const replica = document.createElement('span');
          replica.className = 'sub';
          replica.textContent = 'replica: ' + (object.replicaStatus || 'none');
          providerCell.appendChild(provider);
          providerCell.appendChild(replica);

          const updatedCell = document.createElement('td');
          updatedCell.className = 'mono';
          updatedCell.textContent = object.updatedAt || '—';

          const actionsCell = document.createElement('td');
          const actions = document.createElement('div');
          actions.className = 'row-actions';
          const urlButton = document.createElement('button');
          urlButton.type = 'button';
          urlButton.className = 'btn compact';
          urlButton.textContent = 'Public URL';
          urlButton.addEventListener('click', () => generatePublicURL(object.key));
          actions.appendChild(urlButton);
          const deleteButton = document.createElement('button');
          deleteButton.type = 'button';
          deleteButton.className = 'btn compact danger';
          deleteButton.textContent = 'Delete';
          deleteButton.addEventListener('click', () => openDeleteObjectDialog(object.key));
          actions.appendChild(deleteButton);
          actionsCell.appendChild(actions);

          row.appendChild(nameCell);
          row.appendChild(sizeCell);
          row.appendChild(providerCell);
          row.appendChild(updatedCell);
          row.appendChild(actionsCell);
          objectRows.appendChild(row);
        });
      };

      const loadObjects = async () => {
        if (!objectBucket || !objectRows) return;
        const bucket = objectBucket.value;
        if (!bucket) {
          showObjectStatus('error', 'Select a bucket first.');
          return;
        }
        hideObjectStatus();
        setPublicURL('');
        objectRows.innerHTML = '<tr><td colspan="5"><div class="empty">Loading objects…</div></td></tr>';
        const params = new URLSearchParams({ bucket, prefix: objectPrefix ? objectPrefix.value : '', limit: '500' });
        try {
          const response = await fetch('/admin/api/objects?' + params.toString(), { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await response.json();
          if (!response.ok) throw new Error(payload.detail || 'Could not load objects');
          renderObjects(payload);
        } catch (error) {
          showObjectStatus('error', error && error.message ? error.message : 'Could not load objects');
          objectRows.innerHTML = '<tr><td colspan="5"><div class="empty">Object loading failed.</div></td></tr>';
        }
      };

      const generatePublicURL = async (key) => {
        if (!objectBucket || !key) return;
        showObjectStatus('', 'Generating public URL…');
        const params = new URLSearchParams({ bucket: objectBucket.value, key, expires: objectExpiry ? objectExpiry.value : '900' });
        try {
          const response = await fetch('/admin/api/objects/presign?' + params.toString(), { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await response.json();
          if (!response.ok) throw new Error(payload.detail || 'Could not generate public URL');
          setPublicURL(payload.url);
          showObjectStatus('success', 'Public URL generated for ' + payload.key + '. Expires at ' + payload.expiresAt + '.');
        } catch (error) {
          setPublicURL('');
          showObjectStatus('error', error && error.message ? error.message : 'Could not generate public URL');
        }
      };

      if (objectLoad) objectLoad.addEventListener('click', loadObjects);
      if (objectBucket) objectBucket.addEventListener('change', loadObjects);
      document.querySelectorAll('[data-browse-objects]').forEach((button) => {
        button.addEventListener('click', () => {
          const card = document.getElementById('object-browser-card');
          if (card && typeof card.scrollIntoView === 'function') {
            card.scrollIntoView({ behavior: 'smooth', block: 'start' });
          }
          loadObjects();
        });
      });
      if (objectUp) objectUp.addEventListener('click', () => {
        if (!objectPrefix) return;
        objectPrefix.value = parentPrefix(objectPrefix.value);
        loadObjects();
      });
      if (publicURLCopy) publicURLCopy.addEventListener('click', async () => {
        if (!publicURLField || !publicURLField.value) return;
        try {
          await navigator.clipboard.writeText(publicURLField.value);
          showObjectStatus('success', 'Public URL copied.');
        } catch (_) {
          publicURLField.select();
          showObjectStatus('', 'Copy the selected URL manually.');
        }
      });
      if (deleteObjectConfirmation && deleteObjectSubmit) {
        deleteObjectConfirmation.addEventListener('input', () => {
          deleteObjectSubmit.disabled = deleteObjectConfirmation.value !== deletePhrase;
        });
      }
      if (deleteObjectForm) {
        deleteObjectForm.addEventListener('submit', async (event) => {
          event.preventDefault();
          if (!objectBucket || !pendingDeleteKey || !deleteObjectConfirmation || deleteObjectConfirmation.value !== deletePhrase) {
            showObjectStatus('error', 'Type the exact confirmation phrase before deleting.');
            return;
          }
          if (deleteObjectSubmit) deleteObjectSubmit.disabled = true;
          try {
            const response = await fetch('/admin/api/objects', {
              method: 'DELETE',
              headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
              credentials: 'same-origin',
              body: JSON.stringify({ bucket: objectBucket.value, key: pendingDeleteKey, confirm: deleteObjectConfirmation.value }),
            });
            const raw = await response.text();
            let payload = {};
            try { payload = raw ? JSON.parse(raw) : {}; } catch (_) {}
            if (!response.ok) throw new Error(payload.detail || raw || 'Could not delete object');
            if (deleteObjectDialog && typeof deleteObjectDialog.close === 'function') deleteObjectDialog.close();
            setPublicURL('');
            showObjectStatus('success', 'Deleted ' + pendingDeleteKey + ' permanently.');
            pendingDeleteKey = '';
            await loadObjects();
          } catch (error) {
            showObjectStatus('error', error && error.message ? error.message : 'Could not delete object');
            if (deleteObjectSubmit && deleteObjectConfirmation.value === deletePhrase) deleteObjectSubmit.disabled = false;
          }
        });
      }

      const migrationForm = document.getElementById('migration-form');
      const migrationBucket = document.getElementById('migration-bucket');
      const migrationPrefix = document.getElementById('migration-prefix');
      const migrationSource = document.getElementById('migration-source-provider');
      const migrationTarget = document.getElementById('migration-target-provider');
      const migrationMode = document.getElementById('migration-mode');
      const migrationConfirmLabel = document.getElementById('migration-confirm-label');
      const migrationConfirm = document.getElementById('migration-confirm');
      const migrationSubmit = document.getElementById('migration-submit');
      const migrationStatus = document.getElementById('migration-status');
      const migrationRows = document.getElementById('migration-job-rows');
      const migrationTable = document.getElementById('migration-jobs-table');
      const migrationEmpty = document.getElementById('migration-jobs-empty');
      const migrationPhrase = 'Migrar permanentemente';

      const showMigrationStatus = (kind, message) => {
        if (!migrationStatus) return;
        migrationStatus.hidden = false;
        migrationStatus.className = 'notice' + (kind ? ' ' + kind : '');
        migrationStatus.textContent = message;
      };

      const migrationProgress = (job) => {
        const total = Number(job.total_objects || 0);
        if (total <= 0) return job.status === 'completed' ? 100 : 0;
        return Math.max(0, Math.min(100, Math.round(Number(job.processed_objects || 0) / total * 100)));
      };

      const renderMigrationJobs = (jobs) => {
        if (!migrationRows || !migrationTable || !migrationEmpty) return;
        migrationRows.textContent = '';
        const list = Array.isArray(jobs) ? jobs : [];
        migrationEmpty.hidden = list.length > 0;
        migrationTable.hidden = list.length === 0;
        list.forEach((job) => {
          const row = document.createElement('tr');
          const jobCell = document.createElement('td');
          const id = document.createElement('span');
          id.className = 'mono';
          id.textContent = job.id || '—';
          const mode = document.createElement('span');
          mode.className = 'sub';
          mode.textContent = job.mode || 'copy';
          jobCell.appendChild(id);
          jobCell.appendChild(mode);

          const scopeCell = document.createElement('td');
          const scope = document.createElement('span');
          scope.className = 'mono';
          scope.textContent = (job.bucket || '') + '/' + (job.prefix || '');
          scopeCell.appendChild(scope);

          const routeCell = document.createElement('td');
          const route = document.createElement('span');
          route.className = 'mono';
          route.textContent = (job.source_provider_id || '—') + ' → ' + (job.target_provider_id || '—');
          routeCell.appendChild(route);

          const progressCell = document.createElement('td');
          const bar = document.createElement('div');
          bar.className = 'progress';
          const fill = document.createElement('span');
          fill.style.width = migrationProgress(job) + '%';
          bar.appendChild(fill);
          const label = document.createElement('span');
          label.className = 'progress-label';
          label.textContent = (job.processed_objects || 0) + ' / ' + (job.total_objects || 0) + ' · ok ' + (job.succeeded_objects || 0) + ' · failed ' + (job.failed_objects || 0);
          progressCell.appendChild(bar);
          progressCell.appendChild(label);

          const statusCell = document.createElement('td');
          const pill = document.createElement('span');
          pill.className = 'pill ' + (job.status || '');
          pill.textContent = job.status || 'pending';
          const current = document.createElement('span');
          current.className = 'sub';
          current.textContent = job.current_key || '';
          statusCell.appendChild(pill);
          statusCell.appendChild(current);

          const errorCell = document.createElement('td');
          const lastError = document.createElement('span');
          lastError.className = 'sub';
          lastError.textContent = job.last_error || '';
          errorCell.appendChild(lastError);

          row.appendChild(jobCell);
          row.appendChild(scopeCell);
          row.appendChild(routeCell);
          row.appendChild(progressCell);
          row.appendChild(statusCell);
          row.appendChild(errorCell);
          migrationRows.appendChild(row);
        });
      };

      const refreshMigrationJobs = async () => {
        if (!migrationRows) return;
        try {
          const response = await fetch('/admin/api/migrations?limit=10', { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const jobs = await response.json();
          if (!response.ok) throw new Error(jobs.detail || 'Could not load migration jobs');
          renderMigrationJobs(jobs);
        } catch (_) {}
      };

      const updateMigrationFormState = () => {
        if (!migrationMode || !migrationSubmit) return;
        const isMove = migrationMode.value === 'move';
        if (migrationConfirmLabel) migrationConfirmLabel.hidden = !isMove;
        const sameProvider = migrationSource && migrationTarget && migrationSource.value === migrationTarget.value;
        const missingMoveConfirmation = isMove && (!migrationConfirm || migrationConfirm.value !== migrationPhrase);
        migrationSubmit.disabled = Boolean(sameProvider || missingMoveConfirmation);
      };

      [migrationMode, migrationSource, migrationTarget, migrationConfirm].forEach((field) => {
        if (field) field.addEventListener('input', updateMigrationFormState);
        if (field) field.addEventListener('change', updateMigrationFormState);
      });
      updateMigrationFormState();

      if (migrationForm) {
        migrationForm.addEventListener('submit', async (event) => {
          event.preventDefault();
          updateMigrationFormState();
          if (migrationSubmit && migrationSubmit.disabled) {
            showMigrationStatus('error', migrationMode && migrationMode.value === 'move' ? 'Type the exact move confirmation phrase and choose different providers.' : 'Choose different source and target providers.');
            return;
          }
          if (migrationSubmit) migrationSubmit.disabled = true;
          showMigrationStatus('', 'Creating migration job…');
          try {
            const response = await fetch('/admin/api/migrations', {
              method: 'POST',
              headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
              credentials: 'same-origin',
              body: JSON.stringify({
                bucket: migrationBucket ? migrationBucket.value : '',
                prefix: migrationPrefix ? migrationPrefix.value : '',
                source_provider_id: migrationSource ? migrationSource.value : '',
                target_provider_id: migrationTarget ? migrationTarget.value : '',
                mode: migrationMode ? migrationMode.value : 'copy',
                confirm: migrationConfirm ? migrationConfirm.value : '',
              }),
            });
            const payload = await response.json();
            if (!response.ok) throw new Error(payload.detail || 'Could not create migration job');
            showMigrationStatus('success', 'Migration job ' + payload.id + ' created. Progress will update below.');
            if (migrationConfirm) migrationConfirm.value = '';
            await refreshMigrationJobs();
          } catch (error) {
            showMigrationStatus('error', error && error.message ? error.message : 'Could not create migration job');
          } finally {
            updateMigrationFormState();
          }
        });
      }
      if (migrationRows) {
        window.setInterval(refreshMigrationJobs, 3000);
      }

      const form = document.getElementById('admin-upload-form');
      const status = document.getElementById('upload-status');
      if (!form || !status || !window.fetch || !window.FormData) return;

      const button = form.querySelector('button[type="submit"]');
      const fileInput = form.querySelector('input[type="file"]');

      const showStatus = (kind, message) => {
        status.hidden = false;
        status.className = 'notice' + (kind ? ' ' + kind : '');
        status.textContent = message;
      };

      form.addEventListener('submit', async (event) => {
        event.preventDefault();
        showStatus('', 'Uploading…');
        if (button) button.disabled = true;

        try {
          const response = await fetch(form.action, {
            method: 'POST',
            body: new FormData(form),
            headers: { Accept: 'application/json' },
            credentials: 'same-origin',
          });
          const raw = await response.text();
          let payload = {};
          try { payload = raw ? JSON.parse(raw) : {}; } catch (_) {}

          if (!response.ok) {
            throw new Error(payload.detail || raw || 'Upload failed');
          }

          const routedVia = payload.providerAccountId ? ' via ' + payload.providerAccountId : '';
          showStatus('success', 'Uploaded ' + payload.bucket + '/' + payload.key + ' (' + payload.size + ' bytes)' + routedVia + '.');
          if (fileInput) fileInput.value = '';
        } catch (error) {
          showStatus('error', error && error.message ? error.message : 'Upload failed');
        } finally {
          if (button) button.disabled = false;
        }
      });
    })();
  </script>
</body>
</html>`))
