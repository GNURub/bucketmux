package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
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
	svc  *app.Service
	oidc *oidcAuth
}

const objectDeleteConfirmationPhrase = "Delete permanently"

func NewHandler(svc *app.Service) *Handler {
	return &Handler{svc: svc, oidc: newOIDCAuth(svc.Config.Admin.OIDC, svc.Secrets)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	if h.oidc != nil && h.oidc.enabled() {
		switch path {
		case "/login":
			h.oidc.login(w, r)
			return
		case "/oidc/callback":
			h.oidc.callback(w, r)
			return
		case "/logout":
			h.oidc.logout(w, r)
			return
		}
	}
	if !h.authorized(r) {
		if h.oidc != nil && h.oidc.enabled() && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="BucketMux admin"`)
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "Admin credentials are required")
		return
	}
	if isMutatingMethod(r.Method) && !h.sameOriginRequest(r) {
		writeProblem(w, http.StatusForbidden, "cross-site-request", "Cross-site admin mutations are forbidden")
		return
	}
	if isMutatingMethod(r.Method) {
		limit := h.svc.Config.Server.MaxAdminBodyBytes
		if path == "/upload" {
			limit = h.svc.Config.Server.MaxUploadBytes + (1 << 20)
		} else if path == "/api/wasm-plugins" || path == "/api/wasm-plugins/validate" || path == "/api/declarative/apply" {
			pluginLimit := h.svc.Config.WASMPlugins.MaxModuleBytes*4/3 + (1 << 20)
			if pluginLimit > limit {
				limit = pluginLimit
			}
		}
		if r.ContentLength > limit {
			writeProblem(w, http.StatusRequestEntityTooLarge, "request-too-large", "Request body exceeds the configured limit")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
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
	case path == "/partials/provider-catalog" && r.Method == http.MethodGet:
		h.providerCatalog(w, r)
	case path == "/api/providers":
		h.providers(w, r)
	case strings.HasPrefix(path, "/api/providers/") && strings.HasSuffix(path, "/test"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/providers/"), "/test")
		h.testProvider(w, r, id)
	case strings.HasPrefix(path, "/api/providers/") && strings.HasSuffix(path, "/buckets"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/providers/"), "/buckets")
		h.discoverProviderBuckets(w, r, id)
	case strings.HasPrefix(path, "/api/providers/") && strings.HasSuffix(path, "/quota/reconcile"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/providers/"), "/quota/reconcile")
		h.reconcileProviderQuota(w, r, id)
	case strings.HasPrefix(path, "/api/providers/"):
		h.providerByID(w, r, strings.TrimPrefix(path, "/api/providers/"))
	case path == "/api/inventory-jobs":
		h.inventoryJobs(w, r)
	case path == "/api/repair-jobs":
		h.repairJobs(w, r)
	case path == "/api/access-credentials":
		h.accessCredentials(w, r)
	case strings.HasPrefix(path, "/api/access-credentials/"):
		h.accessCredentialByID(w, r, strings.TrimPrefix(path, "/api/access-credentials/"))
	case path == "/api/trash":
		h.trash(w, r)
	case strings.HasPrefix(path, "/api/trash/"):
		h.trashByID(w, r, strings.TrimPrefix(path, "/api/trash/"))
	case path == "/api/lifecycle/run":
		h.runLifecycle(w, r)
	case path == "/api/placement-plan":
		h.placementPlan(w, r)
	case path == "/api/cost-optimizations":
		h.costOptimizations(w, r)
	case path == "/api/repair":
		h.repairObject(w, r)
	case path == "/api/declarative/apply":
		h.applyDeclarativeConfig(w, r)
	case path == "/openapi.json" || path == "/api/openapi.json":
		h.openapi(w, r)
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
	case path == "/api/provider-quotas":
		h.providerQuotas(w, r)
	case path == "/api/alerts":
		h.alerts(w, r)
	case path == "/api/wasm-plugins/validate":
		h.validateWASMPlugin(w, r)
	case path == "/api/wasm-plugins":
		h.wasmPlugins(w, r)
	case strings.HasPrefix(path, "/api/wasm-plugins/"):
		h.wasmPluginByID(w, r, strings.TrimPrefix(path, "/api/wasm-plugins/"))
	case path == "/api/wasm-plugin-jobs":
		h.wasmPluginJobs(w, r)
	case path == "/api/embeddings/capabilities":
		h.embeddingCapabilities(w, r)
	case path == "/api/embeddings":
		h.embeddings(w, r)
	case path == "/api/embeddings/search":
		h.searchEmbeddings(w, r)
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
	if h.oidc != nil && h.oidc.authorized(r) {
		return true
	}
	if h.svc.Config.Admin.Username == "" || h.svc.Config.Admin.Password == "" {
		return false
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(h.svc.Config.Admin.Username))
	passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(h.svc.Config.Admin.Password))
	return userMatch&passMatch == 1
}

func isMutatingMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func (h *Handler) sameOriginRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" {
		return false
	}
	if publicBase := strings.TrimSpace(h.svc.Config.Server.PublicBaseURL); publicBase != "" {
		expected, err := url.Parse(publicBase)
		return err == nil && strings.EqualFold(parsed.Scheme, expected.Scheme) && strings.EqualFold(parsed.Host, expected.Host)
	}
	host := r.Host
	if forwardedHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		host = forwardedHost
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if forwardedProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		scheme = forwardedProto
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, host)
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
	accessCredentials, err := h.svc.Store.ListAccessCredentials(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	for index := range accessCredentials {
		accessCredentials[index].SecretEncrypted = ""
	}
	inventoryJobs, err := h.svc.Store.ListInventoryJobs(r.Context(), 25)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	repairJobs, err := h.svc.Store.ListRepairJobs(r.Context(), 25)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	trashObjects, err := h.svc.Store.ListTrashObjects(r.Context(), 100)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	costOptimizations, _ := h.svc.CostOptimizations(r.Context())
	providerQuotas, _ := h.svc.ListProviderQuotas(r.Context())
	alerts, _ := h.svc.Store.ListAlerts(r.Context(), domain.AlertStatusOpen, 25)
	wasmPlugins, _ := h.svc.Store.ListWASMPlugins(r.Context(), false)
	wasmPluginJobs, _ := h.svc.Store.ListWASMPluginJobs(r.Context(), 25)
	providerBrands := make(map[string]string, len(providers))
	providerNames := make(map[string]string, len(providers))
	for _, account := range providers {
		providerBrands[account.ID] = providerBrand(account)
		providerNames[account.ID] = account.Name
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTemplate.Execute(w, adminPageData{Providers: providers, ProviderBrands: providerBrands, ProviderNames: providerNames, Buckets: buckets, Usage: usage, Hooks: hooks, HookDeliveries: deliveries, ProviderHealth: health, MigrationJobs: migrations, AuditEvents: auditEvents, AccessCredentials: accessCredentials, InventoryJobs: inventoryJobs, RepairJobs: repairJobs, TrashObjects: trashObjects, CostOptimizations: costOptimizations, ProviderPresets: providerCatalog, ProviderQuotas: providerQuotas, Alerts: alerts, WASMPlugins: wasmPlugins, WASMPluginJobs: wasmPluginJobs, OIDCEnabled: h.svc.Config.Admin.OIDC.Enabled, TotalBytes: totalUsageBytes(usage), Message: message})
}

func (h *Handler) providerCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.ExecuteTemplate(w, "providerCatalog", filterProviderCatalog(r.URL.Query().Get("q"))); err != nil {
		writeProblem(w, http.StatusInternalServerError, "template-error", err.Error())
	}
}

func (h *Handler) uploadObjectFromForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.uploadFormError(w, r, status, "invalid-upload-form", "Invalid upload form: "+err.Error())
		return
	}
	bucket := strings.TrimSpace(r.FormValue("bucket"))
	key := strings.Trim(strings.TrimSpace(r.FormValue("key")), "/")
	file, header, err := r.FormFile("file")
	if err != nil {
		h.uploadFormError(w, r, http.StatusBadRequest, "missing-file", "File is required: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()
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
	if quotaMargin := strings.TrimSpace(r.FormValue("settings_quota_margin_bytes")); quotaMargin != "" {
		settings["quota_margin_bytes"] = quotaMargin
	}
	if monthlyQuota := strings.TrimSpace(r.FormValue("settings_monthly_upload_quota_bytes")); monthlyQuota != "" {
		settings["monthly_upload_quota_bytes"] = monthlyQuota
	}
	if alertThreshold := strings.TrimSpace(r.FormValue("settings_quota_alert_threshold_percent")); alertThreshold != "" {
		settings["quota_alert_threshold_percent"] = alertThreshold
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
	trashRetentionDays, _ := strconv.Atoi(defaultString(r.FormValue("trash_retention_days"), "30"))
	defaultRetentionDays, _ := strconv.Atoi(r.FormValue("default_retention_days"))
	expireAfterDays, _ := strconv.Atoi(r.FormValue("lifecycle_expire_days"))
	bucket := domain.Bucket{Name: strings.TrimSpace(r.FormValue("name")), ReplicationEnabled: len(replicationProviderIDs) > 0, ReplicationProviderIDs: replicationProviderIDs, VersioningEnabled: r.FormValue("versioning_enabled") == "on", TrashEnabled: r.FormValue("trash_enabled") == "on", TrashRetentionDays: trashRetentionDays, ObjectLockEnabled: r.FormValue("object_lock_enabled") == "on", DefaultRetentionMode: strings.ToUpper(r.FormValue("default_retention_mode")), DefaultRetentionDays: defaultRetentionDays}
	if expireAfterDays > 0 {
		bucket.LifecycleRules = []domain.LifecycleRule{{ID: "default-expiration", Prefix: strings.TrimLeft(r.FormValue("lifecycle_prefix"), "/"), ExpireAfterDays: expireAfterDays, Enabled: true}}
	}
	if bucket.ObjectLockEnabled {
		bucket.VersioningEnabled = true
	}
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

func (h *Handler) testProvider(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	health, err := h.svc.TestProviderConnection(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "provider-test-failed", err.Error())
		return
	}
	status := http.StatusOK
	if health.Status == domain.ProviderHealthUnhealthy {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, health)
}

func (h *Handler) discoverProviderBuckets(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	buckets, err := h.svc.DiscoverProviderBuckets(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bucket-discovery-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_account_id": id, "buckets": buckets})
}

func (h *Handler) reconcileProviderQuota(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	quota, err := h.svc.ReconcileProviderQuota(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "quota-reconciliation-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (h *Handler) providerQuotas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	quotas, err := h.svc.ListProviderQuotas(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "quota-list-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, quotas)
}

func (h *Handler) alerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	alerts, err := h.svc.Store.ListAlerts(r.Context(), r.URL.Query().Get("status"), 100)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "alert-list-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

func (h *Handler) inventoryJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := h.svc.Store.ListInventoryJobs(r.Context(), 50)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "inventory-list-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		var req struct {
			ProviderAccountID string `json:"provider_account_id"`
			Bucket            string `json:"bucket"`
			RemoteBucket      string `json:"remote_bucket"`
			Prefix            string `json:"prefix"`
			Mode              string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		job, err := h.svc.CreateInventoryJob(r.Context(), app.CreateInventoryJobInput{ProviderAccountID: req.ProviderAccountID, Bucket: req.Bucket, RemoteBucket: req.RemoteBucket, Prefix: req.Prefix, Mode: req.Mode})
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "inventory-create-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) repairJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := h.svc.Store.ListRepairJobs(r.Context(), 50)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "repair-list-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		var request struct {
			Bucket string `json:"bucket"`
			Prefix string `json:"prefix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		job, err := h.svc.CreateRepairJob(r.Context(), request.Bucket, request.Prefix)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "repair-create-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

type accessCredentialRequest struct {
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Permissions    []string `json:"permissions"`
	BucketPatterns []string `json:"bucket_patterns"`
	PrefixPatterns []string `json:"prefix_patterns"`
	Enabled        bool     `json:"enabled"`
	ExpiresAt      string   `json:"expires_at"`
}

func (request accessCredentialRequest) toAppInput() (app.AccessCredentialInput, error) {
	var expiresAt time.Time
	var err error
	if strings.TrimSpace(request.ExpiresAt) != "" {
		expiresAt, err = time.Parse(time.RFC3339, request.ExpiresAt)
		if err != nil {
			return app.AccessCredentialInput{}, fmt.Errorf("expires_at must be RFC3339: %w", err)
		}
	}
	return app.AccessCredentialInput{Name: request.Name, Role: request.Role, Permissions: request.Permissions, BucketPatterns: request.BucketPatterns, PrefixPatterns: request.PrefixPatterns, Enabled: request.Enabled, ExpiresAt: expiresAt}, nil
}

func (h *Handler) accessCredentials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		credentials, err := h.svc.Store.ListAccessCredentials(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "credential-list-failed", err.Error())
			return
		}
		for index := range credentials {
			credentials[index].SecretEncrypted = ""
		}
		writeJSON(w, http.StatusOK, credentials)
	case http.MethodPost:
		var request accessCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		input, err := request.toAppInput()
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-credential", err.Error())
			return
		}
		created, err := h.svc.CreateAccessCredential(r.Context(), input)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "credential-create-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) accessCredentialByID(w http.ResponseWriter, r *http.Request, path string) {
	id := strings.TrimSuffix(path, "/")
	if strings.HasSuffix(id, "/rotate") {
		id = strings.TrimSuffix(id, "/rotate")
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
			return
		}
		rotated, err := h.svc.RotateAccessCredential(r.Context(), id)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "credential-rotate-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rotated)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var request accessCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		input, err := request.toAppInput()
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-credential", err.Error())
			return
		}
		credential, err := h.svc.UpdateAccessCredential(r.Context(), id, input)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "credential-update-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, credential)
	case http.MethodDelete:
		if err := h.svc.Store.DeleteAccessCredential(r.Context(), id); err != nil {
			writeProblem(w, http.StatusInternalServerError, "credential-delete-failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) trash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	objects, err := h.svc.Store.ListTrashObjects(r.Context(), 200)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "trash-list-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, objects)
}

func (h *Handler) trashByID(w http.ResponseWriter, r *http.Request, path string) {
	id := strings.TrimSuffix(path, "/")
	if strings.HasSuffix(id, "/restore") {
		id = strings.TrimSuffix(id, "/restore")
		if r.Method != http.MethodPost {
			writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
			return
		}
		object, err := h.svc.RestoreTrashObject(r.Context(), id)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "trash-restore-failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, objectResponseFromDomain(object))
		return
	}
	if r.Method != http.MethodDelete {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	if r.URL.Query().Get("confirm") != "Purge permanently" {
		writeProblem(w, http.StatusBadRequest, "invalid-confirmation", `confirmation must exactly match "Purge permanently"`)
		return
	}
	if err := h.svc.PurgeTrashObject(r.Context(), id); err != nil {
		writeProblem(w, http.StatusBadRequest, "trash-purge-failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) runLifecycle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	result, err := h.svc.RunLifecycleOnce(r.Context(), time.Now().UTC())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "lifecycle-run-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) placementPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	size, err := strconv.ParseInt(defaultString(r.URL.Query().Get("size"), "0"), 10, 64)
	if err != nil || size < 0 {
		writeProblem(w, http.StatusBadRequest, "invalid-size", "size must be a non-negative integer")
		return
	}
	plan, err := h.svc.PlanPlacement(r.Context(), r.URL.Query().Get("bucket"), size)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "placement-plan-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h *Handler) costOptimizations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	optimizations, err := h.svc.CostOptimizations(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "cost-analysis-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, optimizations)
}

func (h *Handler) repairObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	var request struct {
		Bucket string `json:"bucket"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
		return
	}
	if request.Bucket == "" || request.Key == "" {
		writeProblem(w, http.StatusBadRequest, "missing-object", "bucket and key are required")
		return
	}
	result, err := h.svc.RepairObject(r.Context(), request.Bucket, request.Key)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "repair-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
		writeProblem(w, http.StatusBadRequest, "invalid-confirmation", fmt.Sprintf("confirmation must exactly match %q", objectDeleteConfirmationPhrase))
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
	Providers         []domain.ProviderAccount
	ProviderBrands    map[string]string
	ProviderNames     map[string]string
	Buckets           []domain.Bucket
	Usage             []domain.ProviderBucketUsage
	Hooks             []domain.Hook
	HookDeliveries    []domain.HookDelivery
	ProviderHealth    []domain.ProviderHealth
	MigrationJobs     []domain.MigrationJob
	AuditEvents       []domain.AuditEvent
	AccessCredentials []domain.AccessCredential
	InventoryJobs     []domain.InventoryJob
	RepairJobs        []domain.RepairJob
	TrashObjects      []domain.TrashRecord
	CostOptimizations []app.CostOptimization
	ProviderPresets   []providerCatalogPreset
	ProviderQuotas    []domain.ProviderQuota
	Alerts            []domain.Alert
	WASMPlugins       []domain.WASMPlugin
	WASMPluginJobs    []domain.WASMPluginJob
	OIDCEnabled       bool
	TotalBytes        int64
	Message           string
}

func providerBrand(account domain.ProviderAccount) string {
	switch account.Kind {
	case domain.ProviderKindLocal:
		return "local"
	case domain.ProviderKindCloudinary:
		return "cloudinary"
	case domain.ProviderKindVercelBlob:
		return "vercel"
	case domain.ProviderKindAzureBlob:
		return "azure"
	case domain.ProviderKindS3Compat:
		endpoint := strings.ToLower(account.Endpoint)
		switch {
		case strings.Contains(endpoint, "amazonaws.com"):
			return "aws"
		case strings.Contains(endpoint, "cloudflarestorage.com"):
			return "cloudflare"
		case strings.Contains(endpoint, "storage.googleapis.com"):
			return "gcs"
		case strings.Contains(endpoint, "backblazeb2.com"):
			return "backblaze"
		case strings.Contains(endpoint, "digitaloceanspaces.com"):
			return "digitalocean"
		case strings.Contains(endpoint, "idrivee2.com"):
			return "idrive"
		case strings.Contains(endpoint, "objectstorage") && strings.Contains(endpoint, "oraclecloud.com"):
			return "oci"
		case strings.Contains(endpoint, "your-objectstorage.com"):
			return "hetzner"
		case strings.Contains(endpoint, "scw.cloud"):
			return "scaleway"
		case strings.Contains(endpoint, "cloud.ovh.net"):
			return "ovh"
		case strings.Contains(endpoint, "linodeobjects.com"):
			return "akamai"
		case strings.Contains(endpoint, "wasabisys.com"):
			return "wasabi"
		case strings.Contains(endpoint, "minio"):
			return "minio"
		default:
			return "custom"
		}
	default:
		return "custom"
	}
}

func providerBrandMark(brand string) string {
	switch brand {
	case "aws":
		return "aws"
	case "cloudflare":
		return "R2"
	case "gcs":
		return "G"
	case "backblaze":
		return "B2"
	case "digitalocean":
		return "DO"
	case "idrive":
		return "e2"
	case "azure":
		return "AZ"
	case "oci":
		return "OCI"
	case "hetzner":
		return "HZ"
	case "scaleway":
		return "SCW"
	case "ovh":
		return "OVH"
	case "akamai":
		return "AKA"
	case "wasabi":
		return "W"
	case "minio":
		return "M"
	case "cloudinary":
		return "C"
	case "vercel":
		return "▲"
	case "local":
		return "LD"
	default:
		return "S3"
	}
}

const theSVGRevision = "7870bc1c5f657d9accbb7f96cc457b8dd3363ee8"

func providerIconURL(brand string) string {
	icons := map[string]string{
		"aws":          "aws/color.svg",
		"cloudflare":   "cloudflare/color.svg",
		"gcs":          "google-cloud/default.svg",
		"backblaze":    "backblaze/default.svg",
		"digitalocean": "digitalocean/default.svg",
		"wasabi":       "wasabi/default.svg",
		"minio":        "minio/default.svg",
		"cloudinary":   "cloudinary/default.svg",
		"vercel":       "vercel/mono.svg",
	}
	path := icons[brand]
	if path == "" {
		return ""
	}
	return "https://cdn.jsdelivr.net/gh/glincker/thesvg@" + theSVGRevision + "/public/icons/" + path
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
	return slices.Contains(values, needle)
}

func parseHookHeaderLines(raw string) map[string]string {
	headers := map[string]string{}
	for line := range strings.SplitSeq(raw, "\n") {
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
	"formatBytes":       formatBytes,
	"join":              joinStrings,
	"progressPct":       migrationProgressPercent,
	"providerBrand":     providerBrand,
	"providerBrandMark": providerBrandMark,
	"providerIconURL":   providerIconURL,
}).Parse(`{{define "providerIcon"}}<span class="provider-icon provider-icon-{{.}}" aria-hidden="true"><span class="provider-icon-fallback">{{providerBrandMark .}}</span>{{with providerIconURL .}}<img src="{{.}}" alt="" loading="lazy" referrerpolicy="no-referrer" onerror="this.hidden=true">{{end}}</span>{{end}}
{{define "providerCatalog"}}{{range .}}<button class="provider-preset" type="button" data-provider-preset="{{.Key}}">{{template "providerIcon" .Brand}}<span class="provider-preset-copy"><strong>{{.Name}}</strong><span>{{.Description}}</span></span></button>{{else}}<p class="empty provider-catalog-empty">No providers match this name.</p>{{end}}{{end}}
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>BucketMux admin</title>
  <script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.10/dist/htmx.min.js" integrity="sha384-Q+Dky3iHVJOr6wUjQ4ulh6uQ76an/t+ak1+PjMVaxRjbZamFLAG+u9InkfjbsEQf" crossorigin="anonymous" defer></script>
  <link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%23171717'/%3E%3Cpath d='M10 8h7.5c4 0 6 1.6 6 4.3 0 1.8-.9 3.1-2.7 3.7 2.1.6 3.2 1.9 3.2 3.9 0 2.8-2.2 4.6-6.2 4.6H10V8Zm6.9 6.5c1.7 0 2.5-.6 2.5-1.8 0-1.1-.8-1.7-2.5-1.7h-2.8v3.5h2.8Zm.4 6.9c1.8 0 2.7-.7 2.7-2s-.9-1.9-2.7-1.9h-3.2v3.9h3.2Z' fill='white'/%3E%3C/svg%3E" />
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
  <style>
    /* Product dashboard redesign: intentionally self-contained for the embedded admin. */
    :root{
      color-scheme:light;
      --bg:#fff;
      --panel:#fff;
      --panel-2:#fafafa;
      --line:#eaeaea;
      --line-soft:#f0f0f0;
      --text:#171717;
      --muted:#666;
      --muted-2:#8f8f8f;
      --accent:#171717;
      --accent-text:#fff;
      --danger:#dc2626;
      --success:#16a34a;
      --warning:#d97706;
      --radius:8px;
      --shadow:0 12px 32px rgba(0,0,0,.12),0 2px 8px rgba(0,0,0,.06);
      --sidebar-width:232px;
    }
    html[data-theme="dark"]{
      color-scheme:dark;
      --bg:#0a0a0a;
      --panel:#0a0a0a;
      --panel-2:#111;
      --line:#292929;
      --line-soft:#1f1f1f;
      --text:#ededed;
      --muted:#a1a1a1;
      --muted-2:#777;
      --accent:#ededed;
      --accent-text:#0a0a0a;
      --danger:#f87171;
      --success:#4ade80;
      --warning:#fbbf24;
      --shadow:0 16px 44px rgba(0,0,0,.52),0 0 0 1px rgba(255,255,255,.08);
    }
    html{scroll-behavior:smooth;background:var(--bg)}
    body{background:var(--bg);color:var(--text);font-family:Geist,Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;letter-spacing:-.006em}
    button,input,select,textarea{font:inherit}
    .app-shell{min-height:100vh;display:grid;grid-template-columns:var(--sidebar-width) minmax(0,1fr)}
    .sidebar{position:fixed;inset:0 auto 0 0;width:var(--sidebar-width);display:flex;flex-direction:column;border-right:1px solid var(--line);background:var(--panel);z-index:30}
    .sidebar-brand{height:64px;padding:0 20px;display:flex;align-items:center;border-bottom:1px solid var(--line)}
    .brand{gap:10px}.logo{width:28px;height:28px;border-radius:6px;background:var(--text);color:var(--bg);box-shadow:none;font-size:13px}.brand h1{font-size:15px;font-weight:650}.brand p{display:none}
    .sidebar-nav{display:grid;gap:2px;padding:16px 10px}.nav-link{display:flex;align-items:center;gap:10px;min-height:38px;padding:8px 10px;border-radius:6px;color:var(--muted);font-size:13px;font-weight:500;text-decoration:none;transition:background .14s,color .14s}.nav-link:hover,.nav-link.active{background:var(--panel-2);color:var(--text)}.nav-link svg{width:16px;height:16px;stroke:currentColor;fill:none;stroke-width:1.75;flex:none}
    .sidebar-footer{margin-top:auto;padding:16px 20px;border-top:1px solid var(--line)}.sidebar-footer .status{padding:0;border:0;background:none;backdrop-filter:none;border-radius:0;font-size:12px;color:var(--muted)}.sidebar-meta{margin:10px 0 0;color:var(--muted-2);font-size:11px}.dot{width:7px;height:7px;box-shadow:none}
    .workspace{grid-column:2;min-width:0}.topbar{position:sticky;top:0;z-index:20;height:64px;margin:0;padding:0 28px;display:flex;flex-direction:row;align-items:center;justify-content:space-between;gap:16px;border-bottom:1px solid var(--line);background:color-mix(in srgb,var(--bg) 92%,transparent);backdrop-filter:blur(16px)}
    .topbar-left,.topbar-actions{display:flex;align-items:center;gap:10px}.icon-btn{appearance:none;width:34px;height:34px;display:grid;place-items:center;border:1px solid var(--line);border-radius:6px;background:var(--panel);color:var(--text);cursor:pointer}.icon-btn.mobile-menu{display:none}.icon-btn:hover{background:var(--panel-2)}.icon-btn svg{width:16px;height:16px;stroke:currentColor;fill:none;stroke-width:1.75}
    .dashboard-search{position:relative;width:min(420px,38vw)}.dashboard-search svg{position:absolute;left:11px;top:50%;width:15px;height:15px;transform:translateY(-50%);stroke:var(--muted-2);fill:none;stroke-width:1.8}.dashboard-search input{height:36px;padding:8px 36px;border-radius:6px;background:var(--panel);font-size:13px}.search-shortcut{position:absolute;right:8px;top:7px;border:1px solid var(--line);border-radius:4px;padding:2px 6px;color:var(--muted-2);font-size:11px;background:var(--panel-2)}
    .topbar-status{display:flex;align-items:center;gap:7px;color:var(--muted);font-size:12px}.topbar-status .dot{background:var(--success)}
    .shell{max-width:1480px;margin:0 auto;padding:34px 32px 64px}.hero{border:0;border-radius:0;padding:0;background:none;box-shadow:none;margin:0 0 24px}.page-heading{display:flex;align-items:flex-start;justify-content:space-between;gap:24px}.eyebrow{display:block;margin-bottom:7px;color:var(--muted);font-size:12px;font-weight:550}.hero h2{margin:0;font-size:30px;line-height:1.15;letter-spacing:-.045em;font-weight:650}.hero p{margin:7px 0 0;max-width:680px;color:var(--muted);font-size:14px;line-height:1.6}.hero-actions{justify-content:flex-end;margin:0}.stats{grid-template-columns:repeat(4,minmax(0,1fr));gap:0;margin-top:28px;border:1px solid var(--line);border-radius:var(--radius);overflow:hidden}.stat{position:relative;min-height:112px;padding:20px;border:0;border-right:1px solid var(--line);border-radius:0;background:var(--panel)}.stat:last-child{border-right:0}.stat strong{font-size:25px;font-weight:600;letter-spacing:-.04em}.stat span{display:block;margin-bottom:14px;color:var(--muted);font-size:12px;text-transform:none;letter-spacing:0}.stat small{display:block;margin-top:8px;color:var(--muted-2);font-size:11px}
    .grid{grid-template-columns:minmax(0,1fr) 268px;gap:20px}.stack{gap:20px}.card{scroll-margin-top:84px;border:1px solid var(--line);border-radius:var(--radius);background:var(--panel);box-shadow:none}.card[hidden]{display:none}.card-header{min-height:66px;padding:16px 18px;border-bottom:1px solid var(--line);align-items:center}.card-title{font-size:14px;font-weight:600}.card-desc{margin-top:4px;color:var(--muted);font-size:12px}.card-body{padding:18px}.card-header-actions{align-items:center}
    .provider-identity{display:flex;align-items:center;gap:10px;min-width:150px}.provider-identity-copy{display:grid;min-width:0}.provider-icon{position:relative;display:inline-grid;place-items:center;flex:0 0 auto;width:30px;height:30px;overflow:hidden;border:1px solid var(--line);border-radius:7px;background:#fff;color:#171717;font-size:9px;font-weight:750;letter-spacing:-.04em}.provider-icon img{position:absolute;inset:5px;width:calc(100% - 10px);height:calc(100% - 10px);object-fit:contain}.provider-icon-fallback{line-height:1}.provider-icon-custom{background:#171717;color:#fff}.provider-icon-local{background:var(--panel-2);color:var(--text)}.provider-icon-vercel{background:#fff}.provider-icon-cloudflare img,.provider-icon-cloudinary img,.provider-icon-gcs img{inset:4px;width:calc(100% - 8px);height:calc(100% - 8px)}
    .provider-catalog-intro{margin-bottom:16px;color:var(--muted);font-size:12px;line-height:1.55}.provider-catalog{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.provider-preset{appearance:none;display:flex;align-items:center;gap:12px;min-height:66px;padding:12px;border:1px solid var(--line);border-radius:8px;background:var(--panel);color:var(--text);text-align:left;cursor:pointer;transition:border-color .14s,background .14s}.provider-preset:hover{border-color:color-mix(in srgb,var(--text) 42%,var(--line));background:var(--panel-2)}.provider-preset:focus-visible{outline:2px solid var(--text);outline-offset:2px}.provider-preset .provider-icon{width:38px;height:38px}.provider-preset-copy{display:grid;gap:3px}.provider-preset-copy strong{font-size:13px;font-weight:600}.provider-preset-copy span{color:var(--muted);font-size:11px;line-height:1.35}.provider-config-summary{display:flex;align-items:center;gap:12px;margin-bottom:18px;padding:12px;border:1px solid var(--line);border-radius:8px;background:var(--panel-2)}.provider-config-summary .provider-icon{width:40px;height:40px}.provider-config-title{display:grid;gap:2px}.provider-config-title strong{font-size:13px}.provider-config-title span{color:var(--muted);font-size:11px}.provider-back{margin-left:auto}.provider-fields-title{margin:20px 0 10px;color:var(--muted);font-size:11px;font-weight:650;text-transform:uppercase;letter-spacing:.06em}.provider-advanced{margin-top:16px;border:1px solid var(--line);border-radius:7px;background:var(--panel)}.provider-advanced summary{padding:12px;cursor:pointer;color:var(--muted);font-size:12px;font-weight:550}.provider-advanced[open] summary{border-bottom:1px solid var(--line);color:var(--text)}.provider-advanced-body{padding:14px}.provider-icon-credit{margin:14px 0 0;color:var(--muted-2);font-size:10px}.provider-icon-credit a{color:inherit}.provider-field[hidden]{display:none}
    .notice{border-color:#f5d08a;background:#fffaf0;color:#92400e;border-radius:6px}.notice.success{border-color:#b7e4c7;background:#f0fdf4;color:#166534}.notice.error{border-color:#fecaca;background:#fef2f2;color:#991b1b}html[data-theme="dark"] .notice{background:#241a08;color:#fcd34d}html[data-theme="dark"] .notice.success{background:#082013;color:#86efac}html[data-theme="dark"] .notice.error{background:#290d0d;color:#fca5a5}
    label{color:var(--text);font-weight:500}.hint{color:var(--muted)}input,select,textarea{border-color:var(--line);border-radius:6px;background:var(--panel);color:var(--text);padding:10px 11px;box-shadow:0 1px 2px rgba(0,0,0,.03)}input:focus,select:focus,textarea:focus{border-color:var(--text);background:var(--panel);box-shadow:0 0 0 2px color-mix(in srgb,var(--text) 12%,transparent)}input::placeholder,textarea::placeholder{color:var(--muted-2)}
    .btn{min-height:34px;border-color:var(--accent);border-radius:6px;background:var(--accent);color:var(--accent-text);padding:8px 12px;font-weight:550;font-size:12px;box-shadow:0 1px 2px rgba(0,0,0,.08);transition:background .12s,border-color .12s,opacity .12s}.btn:hover{transform:none;opacity:.84;background:var(--accent)}.btn.secondary{background:var(--panel);color:var(--text);border-color:var(--line)}.btn.secondary:hover{background:var(--panel-2)}.btn.danger{background:var(--panel);border-color:#fecaca;color:var(--danger)}.btn.danger:hover{background:#fef2f2}html[data-theme="dark"] .btn.danger:hover{background:#290d0d}.btn.compact{min-height:30px;padding:6px 9px}
    .table-wrap{margin:0 -18px -18px}.table th{height:36px;padding:0 14px;background:var(--panel-2);color:var(--muted);font-size:11px;text-transform:none;letter-spacing:0;font-weight:500;white-space:nowrap}.table td{padding:12px 14px;border-top:1px solid var(--line);font-size:12px}.table tr:hover td{background:var(--panel-2)}.table th:first-child,.table td:first-child{padding-left:18px}.table th:last-child,.table td:last-child{padding-right:18px}.name{font-weight:550}.sub{color:var(--muted);font-size:11px}.mono{color:var(--text);font-size:11px}.pill{border:0;background:transparent;border-radius:0;padding:0;color:var(--muted);font-size:11px}.pill:before{width:7px;height:7px}.empty{border-color:var(--line);border-radius:6px;background:var(--panel-2);font-size:12px}
    .browser-toolbar{grid-template-columns:minmax(150px,.65fr) minmax(220px,1.35fr) minmax(140px,.55fr)}.folder-btn{border-color:var(--line);border-radius:6px;background:var(--panel);color:var(--text)}.folder-btn:hover{background:var(--panel-2)}.progress{height:6px;border:0;background:var(--line)}.progress span{background:var(--success)}.code{background:var(--panel-2);border-color:var(--line);border-radius:6px;color:var(--text)}
    .admin-dialog{width:min(680px,calc(100vw - 28px));border-color:var(--line);border-radius:10px;background:var(--panel);color:var(--text);box-shadow:var(--shadow)}.admin-dialog::backdrop{background:rgba(0,0,0,.48);backdrop-filter:blur(2px)}.dialog-header{padding:18px 20px;border-color:var(--line)}.dialog-body{padding:20px}.dialog-close{border-color:var(--line);background:var(--panel);color:var(--text);border-radius:6px}.dialog-close:hover{background:var(--panel-2)}.footer{padding-top:20px;border-top:1px solid var(--line);text-align:left}
    .search-empty{margin-bottom:20px}.filtered-out{display:none!important}
    @media (max-width:1100px){.grid{grid-template-columns:1fr}.grid>aside{grid-row:1}.grid>aside .stack,.grid>aside.stack{display:grid;grid-template-columns:repeat(2,minmax(0,1fr))}.stats{grid-template-columns:repeat(2,minmax(0,1fr))}.stat:nth-child(2){border-right:0}.stat:nth-child(-n+2){border-bottom:1px solid var(--line)}}
    @media (max-width:760px){.app-shell{display:block}.workspace{grid-column:auto}.sidebar{transform:translateX(-100%);transition:transform .18s ease;box-shadow:var(--shadow)}.sidebar.open{transform:translateX(0)}.topbar{height:58px;padding:0 16px}.icon-btn.mobile-menu{display:grid}.dashboard-search{width:min(100%,320px)}.search-shortcut,.topbar-status{display:none}.shell{padding:24px 16px 48px}.page-heading{display:grid}.hero-actions{justify-content:flex-start}.stats{grid-template-columns:1fr}.stat{border-right:0;border-bottom:1px solid var(--line)}.stat:last-child{border-bottom:0}.grid>aside .stack,.grid>aside.stack{grid-template-columns:1fr}.form-grid,.browser-toolbar,.provider-catalog{grid-template-columns:1fr}.card-header{align-items:flex-start}.table-wrap{overflow-x:auto}.hero h2{font-size:25px}}
  </style>
</head>
<body hx-boost="true" hx-target="body" hx-swap="innerHTML show:top">
  <div class="app-shell">
    <aside id="admin-sidebar" class="sidebar" aria-label="Admin navigation">
      <div class="sidebar-brand"><div class="brand"><div class="logo">B</div><div><h1>BucketMux</h1><p>Storage gateway</p></div></div></div>
      <nav class="sidebar-nav">
        <a class="nav-link active" href="#overview"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 13h6V4H4zM14 20h6v-9h-6zM4 20h6v-3H4zM14 7h6V4h-6z"/></svg>Overview</a>
        <a class="nav-link" href="#object-browser-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 7.5h18v12H3zM3 7.5l3-3h5l2 3"/></svg>Objects</a>
        <a class="nav-link" href="#providers-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 8.5a7 7 0 0 1 13.3 2.7A4.5 4.5 0 0 1 18 20H6a4 4 0 0 1-1-7.9"/></svg>Providers</a>
        <a class="nav-link" href="#buckets-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 8h16l-1.5 11h-13zM7 8V5h10v3"/></svg>Buckets</a>
        <a class="nav-link" href="#migration-jobs-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 7h13m0 0-3-3m3 3-3 3M19 17H6m0 0 3-3m-3 3 3 3"/></svg>Migrations</a>
        <a class="nav-link" href="#inventory-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v12H4zM8 10h8M8 14h5"/></svg>Inventory</a>
        <a class="nav-link" href="#repair-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v4m0 10v4M3 12h4m10 0h4M7 7l3 3m4 4 3 3M17 7l-3 3m-4 4-3 3"/></svg>Repair</a>
        <a class="nav-link" href="#security-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l7 3v5c0 4.5-2.8 8-7 10-4.2-2-7-5.5-7-10V6z"/></svg>Access</a>
        <a class="nav-link" href="#protection-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 10V7a5 5 0 0 1 10 0v3M5 10h14v10H5z"/></svg>Protection</a>
        <a class="nav-link" href="#cost-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v18M17 7.5c0-2-2-3-5-3s-5 1-5 3 2 3 5 3 5 1 5 3-2 3-5 3-5-1-5-3"/></svg>Costs</a>
        <a class="nav-link" href="#hooks-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 7V4m8 3V4M6 7h12v5a6 6 0 0 1-12 0zM12 18v3"/></svg>Hooks</a>
        <a class="nav-link" href="#wasm-plugins-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 4h10l3 8-3 8H7l-3-8zM9 9v6m6-6v6"/></svg>WASM plugins</a>
        <a class="nav-link" href="#audit-card"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l7 3v5c0 4.5-2.8 8-7 10-4.2-2-7-5.5-7-10V6z"/></svg>Audit log</a>
      </nav>
      <div class="sidebar-footer"><div class="status"><span class="dot"></span> Admin operational</div><p class="sidebar-meta">Core S3 control plane</p></div>
    </aside>
    <div class="workspace">
    <header class="topbar">
      <div class="topbar-left">
        <button id="sidebar-toggle" class="icon-btn mobile-menu" type="button" aria-label="Open navigation" aria-controls="admin-sidebar" aria-expanded="false"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16"/></svg></button>
        <label class="dashboard-search" aria-label="Filter dashboard"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m16 16 4 4"/></svg><input id="dashboard-search" type="search" placeholder="Filter dashboard…" autocomplete="off"><span class="search-shortcut">/</span></label>
      </div>
      <div class="topbar-actions"><div class="topbar-status"><span class="dot"></span>Operational</div><button id="theme-toggle" class="icon-btn" type="button" aria-label="Toggle color theme"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 15.5A8.5 8.5 0 0 1 8.5 4 8.5 8.5 0 1 0 20 15.5z"/></svg></button></div>
    </header>
    <main class="shell">

    <section id="overview" class="hero">
      <div class="page-heading">
        <div><span class="eyebrow">Storage</span><h2>Storage overview</h2><p>Manage routing, providers, replicas, and object operations from one control plane.</p></div>
        <div class="hero-actions">
          <button class="btn secondary" type="button" data-open-dialog="upload-dialog">Upload object</button>
          <button class="btn secondary" type="button" data-open-dialog="provider-dialog">Add provider</button>
          <button class="btn secondary" type="button" data-open-dialog="bucket-dialog">Add bucket</button>
          <button class="btn" type="button" data-open-dialog="migration-dialog">Start migration</button>
        </div>
      </div>
      <div class="stats">
        <div class="stat"><span>Total usage</span><strong>{{formatBytes .TotalBytes}}</strong><small>Indexed object storage</small></div>
        <div class="stat"><span>Providers</span><strong>{{len .Providers}}</strong><small>Configured backends</small></div>
        <div class="stat"><span>Buckets</span><strong>{{len .Buckets}}</strong><small>Logical S3 buckets</small></div>
        <div class="stat"><span>Migrations</span><strong>{{len .MigrationJobs}}</strong><small>Recent background jobs</small></div>
      </div>
    </section>

    <div id="dashboard-search-empty" class="empty search-empty" hidden>No dashboard sections match this filter.</div>

    {{if .Message}}<div class="notice">{{.Message}}</div>{{end}}

    <div class="grid">
      <div class="stack">
        <section id="providers-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Providers</h2><p class="card-desc">Encrypted credentials. Existing secrets are never displayed.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="provider-dialog">New provider</button></div></div>
          <div class="card-body table-wrap">
            {{if .Providers}}
            <table class="table">
              <thead><tr><th>Provider</th><th>Type</th><th>Target</th><th>Usage</th><th>Status</th><th></th></tr></thead>
              <tbody>
              {{range .Providers}}
                {{$brand := providerBrand .}}
                <tr>
                  <td><span class="provider-identity">{{template "providerIcon" $brand}}<span class="provider-identity-copy"><span class="name">{{.ID}}</span><span class="sub">{{.Name}}</span></span></span></td>
                  <td><span class="pill">{{.Kind}}</span></td>
                  <td><span class="mono">{{if .Endpoint}}{{.Endpoint}}{{else}}{{index .Settings "path"}}{{end}}</span><span class="sub">bucket: {{.Bucket}}</span></td>
                  <td><span class="mono">{{formatBytes .UsedBytes}} / {{formatBytes .CapacityBytes}}</span><span class="sub">priority {{.Priority}}</span></td>
                  <td>{{if .Enabled}}<span class="pill enabled">enabled</span>{{else}}<span class="pill disabled">disabled</span>{{end}}</td>
                  <td><div class="row-actions"><button class="btn compact secondary" type="button" data-test-provider="{{.ID}}">Test</button><button class="btn compact secondary" type="button" data-inventory-provider="{{.ID}}" data-open-dialog="inventory-dialog">Import</button><form method="post" action="/admin/providers/{{.ID}}/delete"><button class="btn compact danger" type="submit">Delete</button></form></div></td>
                </tr>
              {{end}}
              </tbody>
            </table>
            {{else}}<div class="empty">No providers yet. Add your first local, S3-compatible or Cloudinary provider.</div>{{end}}
          </div>
        </section>

        <section id="provider-health-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Provider health</h2><p class="card-desc">Basic configuration, credential, and backend access checks.</p></div></div>
          <div class="card-body table-wrap">
            {{if .ProviderHealth}}
            <table class="table">
              <thead><tr><th>Provider</th><th>Health</th><th>Message</th><th>Latency</th><th>Checked</th></tr></thead>
              <tbody>
              {{range .ProviderHealth}}
                <tr>
                  <td><span class="provider-identity">{{template "providerIcon" (index $.ProviderBrands .ProviderAccountID)}}<span class="provider-identity-copy"><span class="name">{{index $.ProviderNames .ProviderAccountID}}</span><span class="sub mono">{{.ProviderAccountID}}</span></span></span></td>
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

        <section id="provider-quota-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Provider quota</h2><p class="card-desc">Atomic reservations, configured hard limits, and remotely reconciled measurements are shown separately.</p></div></div>
          <div class="card-body table-wrap">
            {{if .ProviderQuotas}}<table class="table"><thead><tr><th>Provider</th><th>Used</th><th>Reserved</th><th>Available</th><th>Monthly uploads</th><th>Source</th></tr></thead><tbody>
            {{range .ProviderQuotas}}<tr><td><span class="mono">{{.ProviderAccountID}}</span></td><td>{{formatBytes .UsedBytes}}</td><td>{{formatBytes .ReservedBytes}}</td><td>{{if eq .AvailableBytes -1}}unlimited{{else}}{{formatBytes .AvailableBytes}}{{end}}</td><td>{{formatBytes .MonthlyUploadedBytes}}{{if gt .MonthlyLimitBytes 0}} / {{formatBytes .MonthlyLimitBytes}}{{end}}</td><td><span class="pill {{if .Reliable}}healthy{{else}}degraded{{end}}">{{if .Source}}{{.Source}}{{else}}not measured{{end}}</span></td></tr>{{end}}
            </tbody></table>{{else}}<div class="empty">No provider quota data yet.</div>{{end}}
          </div>
        </section>

        <section id="alerts-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Active alerts</h2><p class="card-desc">Quota, credentials, provider health, and replication failures.</p></div></div>
          <div class="card-body table-wrap">
            {{if .Alerts}}<table class="table"><thead><tr><th>Severity</th><th>Type</th><th>Target</th><th>Message</th><th>Updated</th></tr></thead><tbody>
            {{range .Alerts}}<tr><td><span class="pill {{if eq .Severity "critical"}}failed{{else}}degraded{{end}}">{{.Severity}}</span></td><td>{{.Type}}</td><td><span class="mono">{{.ProviderAccountID}} {{.Bucket}}/{{.Key}}</span></td><td>{{.Message}}</td><td>{{.UpdatedAt.Format "2006-01-02 15:04 UTC"}}</td></tr>{{end}}
            </tbody></table>{{else}}<div class="empty">No active alerts.</div>{{end}}
          </div>
        </section>

        <section id="usage-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Usage by provider and bucket</h2><p class="card-desc">Space used according to BucketMux's managed object index.</p></div></div>
          <div class="card-body table-wrap">
            {{if .Usage}}
            <table class="table"><thead><tr><th>Provider</th><th>Bucket</th><th>Objects</th><th>Size</th></tr></thead><tbody>{{range .Usage}}<tr><td><span class="provider-identity">{{template "providerIcon" (index $.ProviderBrands .ProviderAccountID)}}<span class="mono">{{.ProviderAccountID}}</span></span></td><td>{{.Bucket}}</td><td>{{.ObjectCount}}</td><td><span class="mono">{{formatBytes .Bytes}}</span></td></tr>{{end}}</tbody></table>
            {{else}}<div class="empty">No indexed objects yet. Upload files to see usage per provider and bucket.</div>{{end}}
          </div>
        </section>

        <section id="wasm-plugins-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">WASM processing pipelines</h2><p class="card-desc">Sandboxed asynchronous transforms and classifiers. Upload modules through <span class="mono">/admin/api/wasm-plugins</span>.</p></div></div>
          <div class="card-body table-wrap">
            {{if .WASMPlugins}}
            <table class="table"><thead><tr><th>Plugin</th><th>Selectors</th><th>Limits</th><th>Module</th><th>Status</th></tr></thead><tbody>
            {{range .WASMPlugins}}<tr><td><span class="name">{{.Name}}</span><span class="sub mono">{{.ID}} · {{.ABIVersion}}</span></td><td><span class="mono">{{.BucketPattern}}/{{.KeyPrefix}}*{{.KeySuffix}}</span><span class="sub">{{join .ContentTypes ", "}}</span></td><td><span class="mono">{{.TimeoutMillis}} ms · {{formatBytes .MemoryLimitBytes}}</span><span class="sub">input/output {{formatBytes .MaxInputBytes}} / {{formatBytes .MaxOutputBytes}}</span><span class="sub">operations: {{if .OperationPolicy.AllowedOperations}}{{join .OperationPolicy.AllowedOperations ", "}} · max {{.OperationPolicy.MaxOperations}}{{else}}none{{end}}</span></td><td><span class="mono">sha256:{{.ModuleSHA256}}</span></td><td>{{if .Enabled}}<span class="pill enabled">enabled</span>{{else}}<span class="pill disabled">disabled</span>{{end}}</td></tr>{{end}}
            </tbody></table>{{else}}<div class="empty">No WASM plugins installed. The Rust and Bun 1.4 examples under <span class="mono">examples/wasm</span> implement the ABI.</div>{{end}}
          </div>
        </section>

        <section id="wasm-plugin-jobs-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">WASM job history</h2><p class="card-desc">Durable claims, retries, source generations, and execution failures across all instances.</p></div></div>
          <div class="card-body table-wrap">
            {{if .WASMPluginJobs}}<table class="table"><thead><tr><th>Job</th><th>Plugin</th><th>Object</th><th>Status</th><th>Attempts</th><th>Result</th></tr></thead><tbody>
            {{range .WASMPluginJobs}}<tr><td><span class="mono">{{.ID}}</span><span class="sub">{{.Event}}</span></td><td><span class="mono">{{.PluginID}}</span></td><td><span class="mono">{{.Bucket}}/{{.Key}}</span></td><td><span class="pill {{.Status}}">{{.Status}}</span></td><td>{{.Attempts}} / {{.MaxAttempts}}</td><td><span class="sub">{{if .LastError}}{{.LastError}}{{else}}{{.FinishedAt}}{{end}}</span></td></tr>{{end}}
            </tbody></table>{{else}}<div class="empty">No WASM jobs yet.</div>{{end}}
          </div>
        </section>

        <section id="hooks-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Hooks</h2><p class="card-desc">Outbound HTTP calls triggered by object events.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="hook-dialog">New hook</button></div></div>
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

        <section id="hook-deliveries-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Hook delivery history</h2><p class="card-desc">Recent deliveries, attempts, and errors. Retries remain pending until completion or exhaustion.</p></div></div>
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

        <section id="buckets-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Buckets</h2><p class="card-desc">Logical buckets exposed by the gateway.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="bucket-dialog">New bucket</button><button class="btn compact secondary" type="button" data-open-dialog="upload-dialog">Upload object</button></div></div>
          <div class="card-body">
            {{if .Buckets}}
            <div class="table-wrap"><table class="table"><thead><tr><th>Name</th><th>Replication targets</th><th>Protection</th></tr></thead><tbody>{{range .Buckets}}<tr><td><span class="name">{{.Name}}</span></td><td><span class="mono">{{if .ReplicationProviderIDs}}{{join .ReplicationProviderIDs ", "}}{{else}}none{{end}}</span></td><td><span class="sub">{{if .VersioningEnabled}}versioned · {{end}}{{if .TrashEnabled}}trash {{.TrashRetentionDays}}d · {{end}}{{if .ObjectLockEnabled}}object lock{{end}}{{if and (not .VersioningEnabled) (not .TrashEnabled) (not .ObjectLockEnabled)}}standard{{end}}</span></td></tr>{{end}}</tbody></table></div>
            {{else}}<div class="empty">No buckets yet. Create a logical bucket to expose it through the S3 API.</div>
            {{end}}
          </div>
        </section>

        <section id="object-browser-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Object browser</h2><p class="card-desc">Browse indexed objects and generate signed public URLs served through BucketMux.</p></div><div class="card-header-actions"><button class="btn compact secondary" type="button" data-open-dialog="upload-dialog">Upload object</button></div></div>
          <div class="card-body">
            {{if .Buckets}}
            <div class="browser-toolbar">
              <label>Bucket
                <select id="object-browser-bucket">
                  {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
                </select>
              </label>
              <label>Prefix <span class="hint">browse folders using /</span><input id="object-browser-prefix" placeholder="uploads/"></label>
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
              <label>Public presigned URL <span class="hint">anyone with this URL can read the object until it expires</span><textarea id="object-public-url" readonly></textarea></label>
              <div class="actions"><a id="object-public-url-open" class="btn" href="#" target="_blank" rel="noopener">Open URL</a><button id="object-public-url-copy" class="btn secondary" type="button">Copy URL</button></div>
            </div>
            {{else}}<div class="empty">Create a bucket first, then upload objects to browse them.</div>{{end}}
          </div>
        </section>

        <section id="migration-jobs-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Migration jobs</h2><p class="card-desc">History and progress for bucket or prefix migrations between providers.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="migration-dialog">New migration</button></div></div>
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

        <section id="inventory-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Remote inventory</h2><p class="card-desc">Discover and import objects that already exist outside BucketMux. Reconcile reports drift without deleting data.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="inventory-dialog">Run inventory</button></div></div>
          <div class="card-body table-wrap">
            {{if .InventoryJobs}}<table class="table"><thead><tr><th>Job</th><th>Scope</th><th>Mode</th><th>Discovered</th><th>Imported</th><th>Missing</th><th>Status</th></tr></thead><tbody>{{range .InventoryJobs}}<tr><td><span class="mono">{{.ID}}</span></td><td><span class="mono">{{.ProviderAccountID}} · {{.RemoteBucket}}/{{.Prefix}}</span><span class="sub">logical bucket: {{.Bucket}}</span></td><td>{{.Mode}}</td><td>{{.DiscoveredObjects}}</td><td>{{.ImportedObjects}}</td><td>{{.MissingObjects}}</td><td><span class="pill {{.Status}}">{{.Status}}</span><span class="sub">{{.LastError}}</span></td></tr>{{end}}</tbody></table>{{else}}<div class="empty">No inventory jobs yet. Test a provider, discover its buckets, then import or reconcile.</div>{{end}}
          </div>
        </section>

        <section id="repair-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Integrity and auto-repair</h2><p class="card-desc">Durable background scans verify every indexed primary and restore unreadable objects from a healthy replica. Jobs are safe across multiple BucketMux instances.</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="repair-dialog">Run repair scan</button></div></div>
          <div class="card-body table-wrap">
            {{if .RepairJobs}}<table class="table"><thead><tr><th>Job</th><th>Scope</th><th>Checked</th><th>Repaired</th><th>Failed</th><th>Status</th></tr></thead><tbody>{{range .RepairJobs}}<tr><td><span class="mono">{{.ID}}</span></td><td><span class="mono">{{.Bucket}}/{{.Prefix}}</span><span class="sub">{{.CurrentKey}}</span></td><td>{{.CheckedObjects}}</td><td>{{.RepairedObjects}}</td><td>{{.FailedObjects}}</td><td><span class="pill {{.Status}}">{{.Status}}</span><span class="sub">{{.LastError}}</span></td></tr>{{end}}</tbody></table>{{else}}<div class="empty">No integrity scans yet. Configure replication, then schedule a scan to verify recoverability.</div>{{end}}
          </div>
        </section>

        <section id="security-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Access credentials</h2><p class="card-desc">Scoped S3 keys with roles, bucket/prefix boundaries, rotation and expiry. {{if .OIDCEnabled}}OIDC is enabled for admin access.{{else}}OIDC is available through admin configuration.{{end}}</p></div><div class="card-header-actions"><button class="btn compact" type="button" data-open-dialog="credential-dialog">Create key</button></div></div>
          <div class="card-body table-wrap">
            {{if .AccessCredentials}}<table class="table"><thead><tr><th>Name</th><th>Access key</th><th>Role</th><th>Scope</th><th>Expires</th><th>Status</th><th></th></tr></thead><tbody>{{range .AccessCredentials}}<tr><td><span class="name">{{.Name}}</span><span class="sub mono">{{.ID}}</span></td><td><span class="mono">{{.AccessKey}}</span></td><td><span class="pill">{{.Role}}</span></td><td><span class="mono">{{join .BucketPatterns ", "}}</span><span class="sub">prefix {{join .PrefixPatterns ", "}}</span></td><td><span class="mono">{{if .ExpiresAt.IsZero}}never{{else}}{{.ExpiresAt}}{{end}}</span></td><td>{{if .Enabled}}<span class="pill enabled">enabled</span>{{else}}<span class="pill disabled">disabled</span>{{end}}</td><td><button class="btn compact secondary" type="button" data-rotate-credential="{{.ID}}">Rotate</button></td></tr>{{end}}</tbody></table>{{else}}<div class="empty">No scoped keys yet. The root S3 key from configuration remains active for recovery.</div>{{end}}
          </div>
        </section>

        <section id="protection-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Recoverable trash</h2><p class="card-desc">Objects remain recoverable until their bucket retention period expires. Lifecycle purges expired entries.</p></div><div class="card-header-actions"><button id="lifecycle-run" class="btn compact secondary" type="button">Run lifecycle</button></div></div>
          <div class="card-body table-wrap">
            {{if .TrashObjects}}<table class="table"><thead><tr><th>Object</th><th>Provider</th><th>Deleted</th><th>Purge after</th><th></th></tr></thead><tbody>{{range .TrashObjects}}<tr><td><span class="name">{{.Object.Bucket}}/{{.Object.Key}}</span><span class="sub">{{formatBytes .Object.Size}}</span></td><td><span class="mono">{{.Object.ProviderAccountID}}</span></td><td><span class="mono">{{.DeletedAt}}</span></td><td><span class="mono">{{.PurgeAfter}}</span></td><td><button class="btn compact secondary" type="button" data-restore-trash="{{.ID}}">Restore</button></td></tr>{{end}}</tbody></table>{{else}}<div class="empty">Trash is empty. Enable it per bucket to make normal deletes recoverable.</div>{{end}}
          </div>
        </section>

        <section id="cost-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Cost optimizer</h2><p class="card-desc">Estimated opportunities based on configured storage price and available capacity. Review before starting a migration.</p></div><div class="card-header-actions"><button class="btn compact secondary" type="button" data-open-dialog="placement-dialog">Simulate placement</button></div></div>
          <div class="card-body table-wrap">
            {{if .CostOptimizations}}<table class="table"><thead><tr><th>From</th><th>To</th><th>Data</th><th>Estimated saving / month</th></tr></thead><tbody>{{range .CostOptimizations}}<tr><td><span class="mono">{{.SourceProviderID}}</span></td><td><span class="mono">{{.TargetProviderID}}</span></td><td>{{formatBytes .Bytes}}</td><td><span class="name">{{printf "$%.2f" .EstimatedMonthlySaving}}</span></td></tr>{{end}}</tbody></table>{{else}}<div class="empty">Add cost per GB/month to provider routing settings to receive optimization suggestions.</div>{{end}}
          </div>
        </section>

        <section id="audit-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Audit log</h2><p class="card-desc">Destructive, move, and scoped WASM bucket operations.</p></div></div>
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
        <section id="action-center-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Action center</h2><p class="card-desc">Keep the dashboard focused: forms open only when requested.</p></div></div>
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

        <section id="quick-test-card" class="card" data-search-section>
          <div class="card-header"><div><h2 class="card-title">Quick test</h2><p class="card-desc">Using the default local development credentials.</p></div></div>
          <div class="card-body"><pre class="code">curl -X PUT http://localhost:8080/images/demo.txt \
  -H 'X-S3LS-Access-Key: local-access-key' \
  -H 'X-S3LS-Secret-Key: local-secret-key' \
  --data 'hello world'</pre></div>
        </section>
      </aside>
    </div>

    <dialog id="provider-dialog" class="admin-dialog" aria-labelledby="provider-dialog-title">
      <div class="dialog-header"><div><h2 id="provider-dialog-title" class="card-title">Choose a provider</h2><p id="provider-dialog-description" class="card-desc">Start with a tested preset, then enter the credentials for your account.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <div id="provider-catalog-view">
          <p class="provider-catalog-intro">Provider presets fill compatible endpoints and defaults automatically. Credentials remain encrypted and are never shown again.</p>
          <label>Filter providers by name<input id="provider-catalog-filter" type="search" name="q" placeholder="Azure, Hetzner, OCI…" autocomplete="off" hx-get="/admin/partials/provider-catalog" hx-trigger="input changed delay:180ms, search" hx-target="#provider-catalog" hx-swap="innerHTML"></label>
          <div id="provider-catalog" class="provider-catalog">
            {{template "providerCatalog" .ProviderPresets}}
          </div>
          <p class="provider-icon-credit">Brand icons provided by <a href="https://thesvg.org/" target="_blank" rel="noopener noreferrer">theSVG</a>. Trademarks belong to their respective owners.</p>
        </div>

        <div id="provider-config-view" hidden>
          <div class="provider-config-summary">
            <span id="selected-provider-icon" class="provider-icon provider-icon-custom" aria-hidden="true"><span id="selected-provider-icon-fallback" class="provider-icon-fallback">S3</span><img id="selected-provider-icon-image" alt="" referrerpolicy="no-referrer" hidden></span>
            <span class="provider-config-title"><strong id="selected-provider-name">Custom S3-compatible</strong><span id="selected-provider-summary">Configure your storage account.</span></span>
            <button id="provider-back" class="btn secondary compact provider-back" type="button">Change</button>
          </div>
          <form id="provider-form" method="post" action="/admin/providers">
            <input id="provider-kind" type="hidden" name="kind" value="s3-compatible">
            <div class="form-grid one">
              <label>Account ID <span class="hint">unique inside BucketMux</span><input id="provider-id" required name="id" placeholder="r2-main"></label>
              <label>Display name<input id="provider-name" name="name" placeholder="Cloudflare R2 main"></label>
              <label>Remote bucket<input id="provider-bucket" required name="bucket" placeholder="images"></label>

              <div class="provider-fields-title">Connection</div>
              <label class="provider-field" data-provider-field="endpoint">Endpoint<input id="provider-endpoint" name="endpoint" type="url" placeholder="https://s3.example.com"></label>
              <label class="provider-field" data-provider-field="region">Region<input id="provider-region" name="region" value="auto"></label>
              <label class="provider-field" data-provider-field="access-key"><span id="provider-access-label">Access key</span><input id="provider-access-key" name="access_key" autocomplete="off"></label>
              <label class="provider-field" data-provider-field="secret-key"><span id="provider-secret-label">Secret key</span> <span class="hint">leave empty when updating to preserve it</span><input id="provider-secret-key" name="secret_key" type="password" autocomplete="new-password" placeholder="Enter credential"></label>
              <label class="provider-field" data-provider-field="local-path">Local path<input id="provider-local-path" name="settings_path" placeholder="./data/local-provider"></label>
              <label class="provider-field" data-provider-field="cloud-name">Cloud name<input id="provider-cloud-name" name="settings_cloud_name" placeholder="my-cloud"></label>
              <label class="provider-field" data-provider-field="vercel-access">Blob access<select id="provider-vercel-access" name="settings_vercel_access"><option value="public">public</option><option value="private">private</option></select></label>
              <label class="provider-field" data-provider-field="vercel-store">Store ID <span class="hint">optional when included in the token</span><input id="provider-vercel-store" name="settings_vercel_store_id" placeholder="store-id"></label>

              <details class="provider-advanced">
                <summary>Advanced routing and capacity</summary>
                <div class="provider-advanced-body form-grid one">
                  <div class="form-grid">
                    <label>Capacity bytes<input id="provider-capacity" name="capacity_bytes" type="number" value="10737418240"></label>
                    <label>Priority<input id="provider-priority" name="priority" type="number" value="100"></label>
                  </div>
                  <label>Cost per GB/month <span class="hint">lower cost wins on priority tie</span><input name="settings_cost_per_gb_month" placeholder="0.015"></label>
                  <label>Max object size bytes <span class="hint">skip this provider for larger objects</span><input name="settings_max_object_size_bytes" type="number" placeholder="104857600"></label>
                  <label>Min free bytes <span class="hint">reserve capacity headroom</span><input name="settings_min_free_bytes" type="number" placeholder="1073741824"></label>
                  <label>Atomic quota margin bytes <span class="hint">never reserve the final safety margin</span><input name="settings_quota_margin_bytes" type="number" min="0" placeholder="536870912"></label>
                  <label>Monthly upload quota bytes <span class="hint">resets atomically each UTC month</span><input name="settings_monthly_upload_quota_bytes" type="number" min="0" placeholder="107374182400"></label>
                  <label>Quota alert threshold %<input name="settings_quota_alert_threshold_percent" type="number" min="1" max="100" value="85"></label>
                  <label class="checkbox"><input type="checkbox" name="enabled" checked> Enabled</label>
                </div>
              </details>
            </div>
            <div class="actions"><button class="btn" type="submit">Save provider</button><button id="provider-reset" class="btn secondary" type="button">Reset fields</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
          </form>
        </div>
      </div>
    </dialog>

    <dialog id="bucket-dialog" class="admin-dialog" aria-labelledby="bucket-dialog-title">
      <div class="dialog-header"><div><h2 id="bucket-dialog-title" class="card-title">Create / update bucket</h2><p class="card-desc">Configure logical buckets and their replication providers.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form method="post" action="/admin/buckets">
          <div class="form-grid one">
            <label>Bucket name<input required name="name" placeholder="images"></label>
            <label>Replication providers <span class="hint">optional; select zero or more replication targets</span>
              <select name="replication_provider_ids" multiple size="5">
                {{range .Providers}}<option value="{{.ID}}">{{.ID}} — {{.Name}}</option>{{end}}
              </select>
            </label>
            <div class="provider-fields-title">Data protection</div>
            <label class="checkbox"><input type="checkbox" name="versioning_enabled"> Version every overwrite and support S3 delete markers</label>
            <label class="checkbox"><input type="checkbox" name="trash_enabled"> Keep deleted objects in recoverable trash</label>
            <label>Trash retention days<input name="trash_retention_days" type="number" min="1" value="30"></label>
            <label class="checkbox"><input type="checkbox" name="object_lock_enabled"> Enable Object Lock <span class="hint">also enables versioning</span></label>
            <div class="form-grid"><label>Default retention mode<select name="default_retention_mode"><option value="">None</option><option value="GOVERNANCE">Governance</option><option value="COMPLIANCE">Compliance</option></select></label><label>Default retention days<input name="default_retention_days" type="number" min="0" value="0"></label></div>
            <div class="provider-fields-title">Lifecycle</div>
            <div class="form-grid"><label>Prefix<input name="lifecycle_prefix" placeholder="logs/"></label><label>Expire current objects after days<input name="lifecycle_expire_days" type="number" min="0" value="0"></label></div>
          </div>
          <div class="actions"><button class="btn" type="submit">Save bucket settings</button><button class="btn secondary" type="reset">Reset</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="upload-dialog" class="admin-dialog" aria-labelledby="upload-dialog-title">
      <div class="dialog-header"><div><h2 id="upload-dialog-title" class="card-title">Upload object</h2><p class="card-desc">Upload a file to a logical bucket using normal BucketMux routing.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="admin-upload-form" method="post" action="/admin/upload" enctype="multipart/form-data">
          <div class="form-grid one">
            <label>Bucket
              <select name="bucket" required>
                {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
              </select>
            </label>
            <label>Object key <span class="hint">optional; uses the file name when empty</span><input name="key" placeholder="uploads/photo.jpg"></label>
            <label>File<input required name="file" type="file"></label>
          </div>
          <div id="upload-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button class="btn" type="submit">Upload file</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="hook-dialog" class="admin-dialog" aria-labelledby="hook-dialog-title">
      <div class="dialog-header"><div><h2 id="hook-dialog-title" class="card-title">Add / update HTTP hook</h2><p class="card-desc">Runs after BucketMux confirms the change in its object index.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form method="post" action="/admin/hooks">
          <div class="form-grid one">
            <label>ID<input required name="id" placeholder="notify-api"></label>
            <label>Name<input name="name" placeholder="Notify API"></label>
            <label>URL<input required name="url" type="url" placeholder="https://example.com/bucketmux/hooks"></label>
            <label>Method<select name="method"><option value="POST">POST</option><option value="PUT">PUT</option><option value="PATCH">PATCH</option><option value="GET">GET</option></select></label>
            <label>Secret headers <span class="hint">one per line as Header-Name: value; encrypted and never displayed again</span><textarea name="headers" placeholder="X-Webhook-Secret: super-secret&#10;Authorization: Bearer token"></textarea></label>
            <label class="checkbox"><input type="checkbox" name="events" value="object.created" checked> object.created</label>
            <label class="checkbox"><input type="checkbox" name="events" value="object.deleted"> object.deleted</label>
            <label class="checkbox"><input type="checkbox" name="enabled" checked> Enabled</label>
          </div>
          <div class="actions"><button class="btn" type="submit">Save hook</button><button class="btn secondary" type="reset">Reset</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="migration-dialog" class="admin-dialog" aria-labelledby="migration-dialog-title">
      <div class="dialog-header"><div><h2 id="migration-dialog-title" class="card-title">Migrate bucket/prefix</h2><p class="card-desc">Create a background job to copy or move objects between providers.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="migration-form">
          <div class="form-grid one">
            <label>Bucket
              <select id="migration-bucket" required>
                {{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
              </select>
            </label>
            <label>Prefix <span class="hint">optional; empty migrates the entire bucket</span><input id="migration-prefix" placeholder="uploads/2026/"></label>
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
            <label id="migration-confirm-label" hidden>Move confirmation <span class="hint">type exactly: Migrate permanently</span><input id="migration-confirm" autocomplete="off" placeholder="Migrate permanently"></label>
          </div>
          <div id="migration-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button id="migration-submit" class="btn" type="submit">Start migration job</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="inventory-dialog" class="admin-dialog" aria-labelledby="inventory-dialog-title">
      <div class="dialog-header"><div><h2 id="inventory-dialog-title" class="card-title">Import or reconcile remote objects</h2><p class="card-desc">Test credentials and inspect remote buckets before creating a durable inventory job.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="inventory-form">
          <div class="form-grid one">
            <label>Provider<select id="inventory-provider" required>{{range .Providers}}<option value="{{.ID}}">{{.ID}} — {{.Name}}</option>{{end}}</select></label>
            <div class="actions"><button id="inventory-test" class="btn secondary compact" type="button">Test connection</button><button id="inventory-discover" class="btn secondary compact" type="button">Discover buckets</button></div>
            <label>Logical BucketMux bucket<select id="inventory-logical-bucket" required>{{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></label>
            <label>Remote provider bucket<input id="inventory-bucket" required list="inventory-bucket-options" placeholder="upstream-images"><datalist id="inventory-bucket-options"></datalist></label>
            <label>Prefix <span class="hint">optional</span><input id="inventory-prefix" placeholder="archive/"></label>
            <label>Mode<select id="inventory-mode"><option value="import">Import missing objects</option><option value="reconcile">Import and report indexed objects missing remotely</option></select></label>
          </div>
          <div id="inventory-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button class="btn" type="submit">Start inventory</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="credential-dialog" class="admin-dialog" aria-labelledby="credential-dialog-title">
      <div class="dialog-header"><div><h2 id="credential-dialog-title" class="card-title">Create scoped S3 key</h2><p class="card-desc">The secret is displayed once. Store it in your secret manager before closing.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="credential-form">
          <div class="form-grid one">
            <label>Name<input id="credential-name" required placeholder="Media read-only"></label>
            <label>Role<select id="credential-role"><option value="read-write">Read and write</option><option value="read-only">Read only</option><option value="admin">S3 administrator</option></select></label>
            <label>Bucket patterns <span class="hint">comma-separated globs</span><input id="credential-buckets" value="*" placeholder="assets-*, backups"></label>
            <label>Prefix patterns <span class="hint">comma-separated globs</span><input id="credential-prefixes" value="*" placeholder="public/*"></label>
            <label>Expires at <span class="hint">optional, RFC3339</span><input id="credential-expires" type="datetime-local"></label>
            <label class="checkbox"><input id="credential-enabled" type="checkbox" checked> Enabled</label>
          </div>
          <div id="credential-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div id="credential-secret-panel" class="public-url-panel" hidden><label>Secret key<textarea id="credential-secret" readonly></textarea></label><div class="actions"><button id="credential-copy" class="btn secondary" type="button">Copy credentials</button></div></div>
          <div class="actions"><button class="btn" type="submit">Create key</button><button class="btn secondary" type="button" data-dialog-close>Close</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="repair-dialog" class="admin-dialog" aria-labelledby="repair-dialog-title">
      <div class="dialog-header"><div><h2 id="repair-dialog-title" class="card-title">Run integrity and repair scan</h2><p class="card-desc">Every matching primary is checked. Missing or unreadable data is restored only when a healthy replica is available.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="repair-form">
          <div class="form-grid one">
            <label>Bucket<select id="repair-bucket" required>{{range .Buckets}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></label>
            <label>Prefix <span class="hint">optional</span><input id="repair-prefix" placeholder="archive/"></label>
          </div>
          <div id="repair-status" class="notice" hidden style="margin-top:16px;margin-bottom:0"></div>
          <div class="actions"><button class="btn" type="submit">Start durable scan</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <dialog id="placement-dialog" class="admin-dialog" aria-labelledby="placement-dialog-title">
      <div class="dialog-header"><div><h2 id="placement-dialog-title" class="card-title">Simulate object placement</h2><p class="card-desc">Preview eligibility, policy constraints, capacity and monthly storage cost without uploading.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="placement-form"><div class="form-grid"><label>Bucket<input id="placement-bucket" placeholder="images"></label><label>Object size bytes<input id="placement-size" type="number" min="0" value="104857600"></label></div><div class="actions"><button class="btn" type="submit">Simulate</button></div></form>
        <div id="placement-status" class="notice" hidden style="margin-top:16px"></div><div id="placement-results" class="table-wrap" style="margin-top:16px"></div>
      </div>
    </dialog>

    <dialog id="delete-object-dialog" class="admin-dialog" aria-labelledby="delete-object-dialog-title">
      <div class="dialog-header"><div><h2 id="delete-object-dialog-title" class="card-title">Delete object</h2><p class="card-desc">Versioned buckets create a delete marker; trash-enabled buckets keep a recoverable copy. Standard buckets delete immediately.</p></div><button class="dialog-close" type="button" data-dialog-close aria-label="Close">×</button></div>
      <div class="dialog-body">
        <form id="delete-object-form">
          <div class="notice error">This changes the live S3 key. To confirm, type exactly: <strong>Delete permanently</strong></div>
          <div class="form-grid one">
            <label>Object<input id="delete-object-key" readonly></label>
            <label>Confirmation<input id="delete-object-confirmation" autocomplete="off" placeholder="Delete permanently"></label>
          </div>
          <div class="actions"><button id="delete-object-submit" class="btn danger" type="submit" disabled>Delete object</button><button class="btn secondary" type="button" data-dialog-close>Cancel</button></div>
        </form>
      </div>
    </dialog>

    <p class="footer">BucketMux · Embedded admin · JSON API available under /admin/api/* · usage: /admin/api/usage</p>
    </main>
    </div>
  </div>
  <script>
    (() => {
      const root = document.documentElement;
      const themeToggle = document.getElementById('theme-toggle');
      let storedTheme = '';
      try { storedTheme = window.localStorage.getItem('bucketmux-admin-theme') || ''; } catch (_) {}
      if (storedTheme === 'dark' || storedTheme === 'light') root.dataset.theme = storedTheme;
      const updateThemeControl = () => {
        if (!themeToggle) return;
        const dark = root.dataset.theme === 'dark';
        themeToggle.setAttribute('aria-pressed', dark ? 'true' : 'false');
        themeToggle.title = dark ? 'Use light theme' : 'Use dark theme';
      };
      updateThemeControl();
      if (themeToggle) themeToggle.addEventListener('click', () => {
        root.dataset.theme = root.dataset.theme === 'dark' ? 'light' : 'dark';
        try { window.localStorage.setItem('bucketmux-admin-theme', root.dataset.theme); } catch (_) {}
        updateThemeControl();
      });

      const sidebar = document.getElementById('admin-sidebar');
      const sidebarToggle = document.getElementById('sidebar-toggle');
      const navLinks = document.querySelectorAll('.nav-link');
      const closeSidebar = () => {
        if (sidebar) sidebar.classList.remove('open');
        if (sidebarToggle) sidebarToggle.setAttribute('aria-expanded', 'false');
      };
      if (sidebarToggle) sidebarToggle.addEventListener('click', () => {
        const open = sidebar && sidebar.classList.toggle('open');
        sidebarToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
      });
      navLinks.forEach((link) => link.addEventListener('click', () => {
        navLinks.forEach((item) => item.classList.toggle('active', item === link));
        closeSidebar();
      }));

      const dashboardSearch = document.getElementById('dashboard-search');
      const dashboardSearchEmpty = document.getElementById('dashboard-search-empty');
      const searchableSections = Array.from(document.querySelectorAll('[data-search-section]'));
      const filterDashboard = () => {
        if (!dashboardSearch) return;
        const query = dashboardSearch.value.trim().toLocaleLowerCase();
        let matches = 0;
        searchableSections.forEach((section) => {
          const visible = !query || section.textContent.toLocaleLowerCase().includes(query);
          section.classList.toggle('filtered-out', !visible);
          if (visible) matches++;
        });
        if (dashboardSearchEmpty) dashboardSearchEmpty.hidden = matches > 0;
      };
      if (dashboardSearch) dashboardSearch.addEventListener('input', filterDashboard);
      document.addEventListener('keydown', (event) => {
        const target = event.target;
        const editing = target && /^(INPUT|SELECT|TEXTAREA)$/.test(target.tagName);
        if (event.key === '/' && !editing && dashboardSearch) {
          event.preventDefault();
          dashboardSearch.focus();
        } else if (event.key === 'Escape' && dashboardSearch && document.activeElement === dashboardSearch) {
          dashboardSearch.value = '';
          filterDashboard();
          dashboardSearch.blur();
        }
      });

      const providerCatalogView = document.getElementById('provider-catalog-view');
      const providerConfigView = document.getElementById('provider-config-view');
      const providerDialogTitle = document.getElementById('provider-dialog-title');
      const providerDialogDescription = document.getElementById('provider-dialog-description');
      const providerForm = document.getElementById('provider-form');
      const providerReset = document.getElementById('provider-reset');
      const providerBack = document.getElementById('provider-back');
      const providerKind = document.getElementById('provider-kind');
      const providerID = document.getElementById('provider-id');
      const providerName = document.getElementById('provider-name');
      const providerBucket = document.getElementById('provider-bucket');
      const providerEndpoint = document.getElementById('provider-endpoint');
      const providerRegion = document.getElementById('provider-region');
      const providerAccessLabel = document.getElementById('provider-access-label');
      const providerSecretLabel = document.getElementById('provider-secret-label');
      const providerLocalPath = document.getElementById('provider-local-path');
      const providerCloudName = document.getElementById('provider-cloud-name');
      const providerVercelAccess = document.getElementById('provider-vercel-access');
      const selectedProviderIcon = document.getElementById('selected-provider-icon');
      const selectedProviderIconImage = document.getElementById('selected-provider-icon-image');
      const selectedProviderIconFallback = document.getElementById('selected-provider-icon-fallback');
      const selectedProviderName = document.getElementById('selected-provider-name');
      const selectedProviderSummary = document.getElementById('selected-provider-summary');
      const providerIconBase = 'https://cdn.jsdelivr.net/gh/glincker/thesvg@7870bc1c5f657d9accbb7f96cc457b8dd3363ee8/public/icons/';
      const providerPresets = {
        aws: { name: 'Amazon S3', summary: 'AWS region and S3 access credentials.', kind: 's3-compatible', id: 'aws-s3', bucket: 'images', region: 'us-east-1', endpointTemplate: 'https://s3.{region}.amazonaws.com', icon: providerIconBase + 'aws/color.svg', mark: 'aws', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        cloudflare: { name: 'Cloudflare R2', summary: 'Paste the S3 API endpoint from your R2 bucket settings.', kind: 's3-compatible', id: 'cloudflare-r2', bucket: 'images', region: 'auto', endpoint: '', endpointPlaceholder: 'https://ACCOUNT_ID.r2.cloudflarestorage.com', icon: providerIconBase + 'cloudflare/color.svg', mark: 'R2', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        gcs: { name: 'Google Cloud Storage', summary: 'Use HMAC interoperability access and secret keys.', kind: 's3-compatible', id: 'google-cloud-storage', bucket: 'images', region: 'auto', endpoint: 'https://storage.googleapis.com', icon: providerIconBase + 'google-cloud/default.svg', mark: 'G', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        backblaze: { name: 'Backblaze B2', summary: 'Use an S3-compatible application key for the selected region.', kind: 's3-compatible', id: 'backblaze-b2', bucket: 'images', region: 'us-west-004', endpointTemplate: 'https://s3.{region}.backblazeb2.com', icon: providerIconBase + 'backblaze/default.svg', mark: 'B2', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        idrive: { name: 'IDrive e2', summary: 'Use the region-specific access keys issued by IDrive e2.', kind: 's3-compatible', id: 'idrive-e2', bucket: 'images', region: 'us-east-1', endpointTemplate: 'https://s3.{region}.idrivee2.com', mark: 'e2', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        azure: { name: 'Microsoft Azure Blob Storage', summary: 'Use the storage account name and one of its account keys.', kind: 'azure-blob', id: 'azure-blob', bucket: 'images', region: 'auto', endpoint: '', endpointPlaceholder: 'https://ACCOUNT.blob.core.windows.net', mark: 'AZ', accessLabel: 'Storage account name', secretLabel: 'Account key', fields: ['endpoint', 'access-key', 'secret-key'] },
        oci: { name: 'Oracle Cloud Infrastructure (OCI) Object Storage', summary: 'Use an OCI Customer Secret Key and the S3 Compatibility endpoint for your namespace.', kind: 's3-compatible', id: 'oci-object-storage', bucket: 'images', region: 'eu-frankfurt-1', endpoint: '', endpointPlaceholder: 'https://NAMESPACE.compat.objectstorage.eu-frankfurt-1.oraclecloud.com', mark: 'OCI', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        digitalocean: { name: 'DigitalOcean Spaces', summary: 'Use a Spaces access key and the region that owns the Space.', kind: 's3-compatible', id: 'digitalocean-spaces', bucket: 'images', region: 'nyc3', endpointTemplate: 'https://{region}.digitaloceanspaces.com', icon: providerIconBase + 'digitalocean/default.svg', mark: 'DO', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        hetzner: { name: 'Hetzner Object Storage', summary: 'Choose the bucket location and use project S3 credentials.', kind: 's3-compatible', id: 'hetzner-object-storage', bucket: 'images', region: 'fsn1', endpointTemplate: 'https://{region}.your-objectstorage.com', mark: 'HZ', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        scaleway: { name: 'Scaleway Object Storage', summary: 'Use a regional S3 endpoint with a Scaleway access and secret key.', kind: 's3-compatible', id: 'scaleway-object-storage', bucket: 'images', region: 'fr-par', endpointTemplate: 'https://s3.{region}.scw.cloud', mark: 'SCW', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        ovh: { name: 'OVHcloud Object Storage', summary: 'Use the exact S3 endpoint shown for the bucket region and storage class.', kind: 's3-compatible', id: 'ovh-object-storage', bucket: 'images', region: 'gra', endpoint: '', endpointPlaceholder: 'https://s3.gra.io.cloud.ovh.net', mark: 'OVH', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        akamai: { name: 'Akamai Connected Cloud', summary: 'Paste the S3 endpoint hostname assigned to the Linode Object Storage bucket.', kind: 's3-compatible', id: 'akamai-object-storage', bucket: 'images', region: 'us-east-1', endpoint: '', endpointPlaceholder: 'https://us-iad-18.linodeobjects.com', mark: 'AKA', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        wasabi: { name: 'Wasabi', summary: 'Use Wasabi access credentials and the bucket region.', kind: 's3-compatible', id: 'wasabi', bucket: 'images', region: 'us-east-1', endpointTemplate: 'https://s3.{region}.wasabisys.com', icon: providerIconBase + 'wasabi/default.svg', mark: 'W', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        minio: { name: 'MinIO', summary: 'Point BucketMux at your self-hosted MinIO API endpoint.', kind: 's3-compatible', id: 'minio', bucket: 'images', region: 'us-east-1', endpoint: 'http://localhost:9000', icon: providerIconBase + 'minio/default.svg', mark: 'M', fields: ['endpoint', 'region', 'access-key', 'secret-key'] },
        cloudinary: { name: 'Cloudinary', summary: 'Enter the cloud name, API key, and API secret.', kind: 'cloudinary', id: 'cloudinary', bucket: 'media', region: 'auto', icon: providerIconBase + 'cloudinary/default.svg', mark: 'C', accessLabel: 'API key', secretLabel: 'API secret', fields: ['access-key', 'secret-key', 'cloud-name'] },
        vercel: { name: 'Vercel Blob', summary: 'A read-write token is enough; the store ID is normally detected.', kind: 'vercel-blob', id: 'vercel-blob', bucket: 'blob', region: 'auto', icon: providerIconBase + 'vercel/mono.svg', mark: '▲', secretLabel: 'Read-write token', fields: ['secret-key', 'vercel-access', 'vercel-store'] },
        local: { name: 'Local disk', summary: 'Store objects on a filesystem path available to this instance.', kind: 'local', id: 'local-disk', bucket: 'images', region: 'auto', localPath: './data/local-provider', mark: 'LD', fields: ['local-path'] },
        custom: { name: 'Custom S3-compatible', summary: 'Configure any path-style S3-compatible endpoint.', kind: 's3-compatible', id: 'custom-s3', bucket: 'images', region: 'auto', endpoint: '', endpointPlaceholder: 'https://s3.example.com', mark: 'S3', fields: ['endpoint', 'region', 'access-key', 'secret-key'] }
      };
      let selectedProviderPreset = null;

      const providerEndpointFor = (preset) => {
        if (!preset) return '';
        if (preset.endpointTemplate) return preset.endpointTemplate.replace('{region}', providerRegion && providerRegion.value ? providerRegion.value : preset.region);
        return preset.endpoint || '';
      };

      const showProviderCatalog = () => {
        selectedProviderPreset = null;
        if (providerCatalogView) providerCatalogView.hidden = false;
        if (providerConfigView) providerConfigView.hidden = true;
        if (providerDialogTitle) providerDialogTitle.textContent = 'Choose a provider';
        if (providerDialogDescription) providerDialogDescription.textContent = 'Start with a tested preset, then enter the credentials for your account.';
      };

      const applyProviderPreset = (presetKey) => {
        const preset = providerPresets[presetKey];
        if (!preset || !providerForm) return;
        selectedProviderPreset = presetKey;
        providerForm.reset();
        document.querySelectorAll('[data-provider-field]').forEach((field) => {
          field.hidden = !preset.fields.includes(field.getAttribute('data-provider-field'));
        });
        if (providerKind) providerKind.value = preset.kind;
        if (providerID) providerID.value = preset.id;
        if (providerName) providerName.value = preset.name;
        if (providerBucket) providerBucket.value = preset.bucket;
        if (providerRegion) providerRegion.value = preset.region;
        if (providerEndpoint) {
          providerEndpoint.value = providerEndpointFor(preset);
          providerEndpoint.placeholder = preset.endpointPlaceholder || providerEndpoint.value || 'https://s3.example.com';
          providerEndpoint.required = preset.fields.includes('endpoint');
        }
        if (providerLocalPath) {
          providerLocalPath.value = preset.localPath || '';
          providerLocalPath.required = preset.fields.includes('local-path');
        }
        if (providerCloudName) {
          providerCloudName.value = '';
          providerCloudName.required = preset.fields.includes('cloud-name');
        }
        if (providerVercelAccess) providerVercelAccess.value = 'public';
        if (providerAccessLabel) providerAccessLabel.textContent = preset.accessLabel || 'Access key';
        if (providerSecretLabel) providerSecretLabel.textContent = preset.secretLabel || 'Secret key';
        if (selectedProviderName) selectedProviderName.textContent = preset.name;
        if (selectedProviderSummary) selectedProviderSummary.textContent = preset.summary;
        if (selectedProviderIcon) selectedProviderIcon.className = 'provider-icon provider-icon-' + presetKey;
        if (selectedProviderIconFallback) selectedProviderIconFallback.textContent = preset.mark;
        if (selectedProviderIconImage) {
          selectedProviderIconImage.hidden = !preset.icon;
          selectedProviderIconImage.src = preset.icon || '';
        }
        if (providerCatalogView) providerCatalogView.hidden = true;
        if (providerConfigView) providerConfigView.hidden = false;
        if (providerDialogTitle) providerDialogTitle.textContent = 'Configure ' + preset.name;
        if (providerDialogDescription) providerDialogDescription.textContent = 'Use the same account ID to update it. Leave an existing secret empty to preserve it.';
        if (providerID) providerID.focus();
      };

      document.addEventListener('click', (event) => {
        const button = event.target && event.target.closest ? event.target.closest('[data-provider-preset]') : null;
        if (button) applyProviderPreset(button.getAttribute('data-provider-preset'));
      });
      if (providerBack) providerBack.addEventListener('click', showProviderCatalog);
      if (providerRegion) providerRegion.addEventListener('input', () => {
        const preset = providerPresets[selectedProviderPreset];
        if (preset && preset.endpointTemplate && providerEndpoint) providerEndpoint.value = providerEndpointFor(preset);
      });
      if (providerReset) providerReset.addEventListener('click', () => {
        if (selectedProviderPreset) applyProviderPreset(selectedProviderPreset);
      });

      const openButtons = document.querySelectorAll('[data-open-dialog]');
      const closeButtons = document.querySelectorAll('[data-dialog-close]');

      openButtons.forEach((button) => {
        button.addEventListener('click', () => {
          const dialog = document.getElementById(button.getAttribute('data-open-dialog'));
          if (!dialog) return;
          if (dialog.id === 'provider-dialog') showProviderCatalog();
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
      const deletePhrase = 'Delete permanently';

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
            showObjectStatus('success', 'Deleted ' + pendingDeleteKey + ' using the bucket protection policy.');
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
      const migrationPhrase = 'Migrate permanently';

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

      const setNotice = (element, kind, message) => {
        if (!element) return;
        element.hidden = false;
        element.className = 'notice' + (kind ? ' ' + kind : '');
        element.textContent = message;
      };
      const readProblem = async (response, fallback) => {
        const raw = await response.text();
        let payload = {};
        try { payload = raw ? JSON.parse(raw) : {}; } catch (_) {}
        if (!response.ok) throw new Error(payload.detail || raw || fallback);
        return payload;
      };

      document.querySelectorAll('[data-test-provider]').forEach((testButton) => {
        testButton.addEventListener('click', async () => {
          const original = testButton.textContent;
          testButton.disabled = true;
          testButton.textContent = 'Testing…';
          try {
            const response = await fetch('/admin/api/providers/' + encodeURIComponent(testButton.dataset.testProvider) + '/test', { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' });
            const payload = await readProblem(response, 'Connection test failed');
            testButton.textContent = payload.status === 'healthy' ? 'Healthy' : payload.status;
            testButton.title = payload.message || '';
          } catch (error) {
            testButton.textContent = 'Failed';
            testButton.title = error.message || 'Connection test failed';
          } finally {
            window.setTimeout(() => { testButton.textContent = original; testButton.disabled = false; }, 4000);
          }
        });
      });

      const inventoryForm = document.getElementById('inventory-form');
      const inventoryProvider = document.getElementById('inventory-provider');
      const inventoryBucket = document.getElementById('inventory-bucket');
      const inventoryLogicalBucket = document.getElementById('inventory-logical-bucket');
      const inventoryPrefix = document.getElementById('inventory-prefix');
      const inventoryMode = document.getElementById('inventory-mode');
      const inventoryStatus = document.getElementById('inventory-status');
      const inventoryOptions = document.getElementById('inventory-bucket-options');
      document.querySelectorAll('[data-inventory-provider]').forEach((importButton) => importButton.addEventListener('click', () => {
        if (inventoryProvider) inventoryProvider.value = importButton.dataset.inventoryProvider || '';
      }));
      const testInventoryProvider = async () => {
        if (!inventoryProvider || !inventoryProvider.value) return;
        setNotice(inventoryStatus, '', 'Testing provider connection…');
        try {
          const response = await fetch('/admin/api/providers/' + encodeURIComponent(inventoryProvider.value) + '/test', { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await readProblem(response, 'Connection test failed');
          setNotice(inventoryStatus, payload.status === 'healthy' ? 'success' : '', (payload.status || 'unknown') + ': ' + (payload.message || 'connection completed'));
        } catch (error) { setNotice(inventoryStatus, 'error', error.message || 'Connection test failed'); }
      };
      const inventoryTest = document.getElementById('inventory-test');
      if (inventoryTest) inventoryTest.addEventListener('click', testInventoryProvider);
      const inventoryDiscover = document.getElementById('inventory-discover');
      if (inventoryDiscover) inventoryDiscover.addEventListener('click', async () => {
        if (!inventoryProvider || !inventoryProvider.value) return;
        setNotice(inventoryStatus, '', 'Discovering remote buckets…');
        try {
          const response = await fetch('/admin/api/providers/' + encodeURIComponent(inventoryProvider.value) + '/buckets', { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await readProblem(response, 'Bucket discovery failed');
          if (inventoryOptions) {
            inventoryOptions.textContent = '';
            (payload.buckets || []).forEach((bucket) => { const option = document.createElement('option'); option.value = bucket.name; inventoryOptions.appendChild(option); });
          }
          if (inventoryBucket && payload.buckets && payload.buckets.length === 1) inventoryBucket.value = payload.buckets[0].name;
          setNotice(inventoryStatus, 'success', 'Found ' + ((payload.buckets || []).length) + ' remote bucket(s).');
        } catch (error) { setNotice(inventoryStatus, 'error', error.message || 'Bucket discovery failed'); }
      });
      if (inventoryForm) inventoryForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        setNotice(inventoryStatus, '', 'Creating durable inventory job…');
        try {
          const response = await fetch('/admin/api/inventory-jobs', { method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ provider_account_id: inventoryProvider ? inventoryProvider.value : '', bucket: inventoryLogicalBucket ? inventoryLogicalBucket.value : '', remote_bucket: inventoryBucket ? inventoryBucket.value : '', prefix: inventoryPrefix ? inventoryPrefix.value : '', mode: inventoryMode ? inventoryMode.value : 'import' }) });
          const payload = await readProblem(response, 'Could not create inventory job');
          setNotice(inventoryStatus, 'success', 'Inventory job ' + payload.id + ' started. Reload to see progress.');
        } catch (error) { setNotice(inventoryStatus, 'error', error.message || 'Could not create inventory job'); }
      });

      const credentialDialog = document.getElementById('credential-dialog');
      const credentialForm = document.getElementById('credential-form');
      const credentialStatus = document.getElementById('credential-status');
      const credentialSecretPanel = document.getElementById('credential-secret-panel');
      const credentialSecret = document.getElementById('credential-secret');
      const csvValues = (value) => String(value || '').split(',').map((item) => item.trim()).filter(Boolean);
      const revealCredential = (payload, rotated) => {
        if (credentialSecretPanel) credentialSecretPanel.hidden = false;
        if (credentialSecret) credentialSecret.value = 'AWS_ACCESS_KEY_ID=' + payload.credential.access_key + '\nAWS_SECRET_ACCESS_KEY=' + payload.secret_key;
        setNotice(credentialStatus, 'success', rotated ? 'Key rotated. The previous secret no longer works.' : 'Key created. Copy this secret now; it will not be shown again.');
      };
      if (credentialForm) credentialForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        const expiresInput = document.getElementById('credential-expires');
        let expiresAt = '';
        if (expiresInput && expiresInput.value) expiresAt = new Date(expiresInput.value).toISOString();
        setNotice(credentialStatus, '', 'Creating key…');
        try {
          const response = await fetch('/admin/api/access-credentials', { method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ name: document.getElementById('credential-name').value, role: document.getElementById('credential-role').value, bucket_patterns: csvValues(document.getElementById('credential-buckets').value), prefix_patterns: csvValues(document.getElementById('credential-prefixes').value), enabled: document.getElementById('credential-enabled').checked, expires_at: expiresAt }) });
          revealCredential(await readProblem(response, 'Could not create key'), false);
        } catch (error) { setNotice(credentialStatus, 'error', error.message || 'Could not create key'); }
      });
      document.querySelectorAll('[data-rotate-credential]').forEach((rotateButton) => rotateButton.addEventListener('click', async () => {
        if (!window.confirm('Rotate this key now? The current secret will stop working immediately.')) return;
        try {
          const response = await fetch('/admin/api/access-credentials/' + encodeURIComponent(rotateButton.dataset.rotateCredential) + '/rotate', { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await readProblem(response, 'Could not rotate key');
          if (credentialDialog && typeof credentialDialog.showModal === 'function' && !credentialDialog.open) credentialDialog.showModal();
          revealCredential(payload, true);
        } catch (error) { window.alert(error.message || 'Could not rotate key'); }
      }));
      const credentialCopy = document.getElementById('credential-copy');
      if (credentialCopy) credentialCopy.addEventListener('click', async () => {
        if (!credentialSecret || !credentialSecret.value) return;
        try { await navigator.clipboard.writeText(credentialSecret.value); setNotice(credentialStatus, 'success', 'Credentials copied.'); } catch (_) { credentialSecret.select(); }
      });

      document.querySelectorAll('[data-restore-trash]').forEach((restoreButton) => restoreButton.addEventListener('click', async () => {
        restoreButton.disabled = true;
        try {
          const response = await fetch('/admin/api/trash/' + encodeURIComponent(restoreButton.dataset.restoreTrash) + '/restore', { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          await readProblem(response, 'Could not restore object');
          window.location.reload();
        } catch (error) { restoreButton.disabled = false; window.alert(error.message || 'Could not restore object'); }
      }));
      const lifecycleRun = document.getElementById('lifecycle-run');
      if (lifecycleRun) lifecycleRun.addEventListener('click', async () => {
        lifecycleRun.disabled = true;
        lifecycleRun.textContent = 'Running…';
        try {
          const response = await fetch('/admin/api/lifecycle/run', { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await readProblem(response, 'Lifecycle run failed');
          lifecycleRun.textContent = 'Expired ' + payload.expired_objects + ' · purged ' + payload.purged_trash;
        } catch (error) { lifecycleRun.textContent = 'Run failed'; lifecycleRun.title = error.message || ''; }
        window.setTimeout(() => { lifecycleRun.disabled = false; lifecycleRun.textContent = 'Run lifecycle'; }, 5000);
      });

      const repairForm = document.getElementById('repair-form');
      const repairStatus = document.getElementById('repair-status');
      if (repairForm) repairForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        setNotice(repairStatus, '', 'Creating durable integrity scan…');
        try {
          const response = await fetch('/admin/api/repair-jobs', { method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify({ bucket: document.getElementById('repair-bucket').value, prefix: document.getElementById('repair-prefix').value }) });
          const payload = await readProblem(response, 'Could not create repair scan');
          setNotice(repairStatus, 'success', 'Repair scan ' + payload.id + ' started. Reload to see progress.');
        } catch (error) { setNotice(repairStatus, 'error', error.message || 'Could not create repair scan'); }
      });

      const placementForm = document.getElementById('placement-form');
      const placementStatus = document.getElementById('placement-status');
      const placementResults = document.getElementById('placement-results');
      if (placementForm) placementForm.addEventListener('submit', async (event) => {
        event.preventDefault();
        setNotice(placementStatus, '', 'Calculating placement…');
        const params = new URLSearchParams({ bucket: document.getElementById('placement-bucket').value, size: document.getElementById('placement-size').value });
        try {
          const response = await fetch('/admin/api/placement-plan?' + params.toString(), { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
          const payload = await readProblem(response, 'Placement simulation failed');
          if (placementResults) {
            placementResults.textContent = '';
            const table = document.createElement('table'); table.className = 'table';
            const head = document.createElement('thead'); head.innerHTML = '<tr><th>Provider</th><th>Eligible</th><th>Remaining</th><th>Projected / month</th></tr>'; table.appendChild(head);
            const body = document.createElement('tbody');
            (payload.providers || []).forEach((provider) => { const row = document.createElement('tr'); [provider.provider_name + (provider.recommended ? ' · recommended' : ''), provider.eligible ? 'yes' : provider.reason, humanBytes(provider.remaining_bytes), '$' + Number(provider.projected_monthly_cost || 0).toFixed(2)].forEach((value) => { const cell = document.createElement('td'); cell.textContent = value; row.appendChild(cell); }); body.appendChild(row); });
            table.appendChild(body); placementResults.appendChild(table);
          }
          setNotice(placementStatus, 'success', 'Placement simulation complete. No data was written.');
        } catch (error) { setNotice(placementStatus, 'error', error.message || 'Placement simulation failed'); }
      });

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
