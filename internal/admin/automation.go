package admin

import (
	"encoding/json"
	"net/http"

	"github.com/gnurub/bucketmux/internal/domain"
)

type declarativeConfig struct {
	Providers []providerRequest `json:"providers"`
	Buckets   []domain.Bucket   `json:"buckets"`
	Hooks     []hookRequest     `json:"hooks"`
}

func (h *Handler) applyDeclarativeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	var request declarativeConfig
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
		return
	}
	for _, provider := range request.Providers {
		account := provider.toDomain()
		if account.ID == "" || account.Kind == "" || account.Bucket == "" {
			writeProblem(w, http.StatusBadRequest, "invalid-provider", "every provider requires id, kind and bucket")
			return
		}
		if err := h.svc.UpsertProviderFromAdmin(r.Context(), account, provider.SecretKey); err != nil {
			writeProblem(w, http.StatusBadRequest, "provider-apply-failed", err.Error())
			return
		}
	}
	for _, bucket := range request.Buckets {
		if bucket.Name == "" {
			writeProblem(w, http.StatusBadRequest, "invalid-bucket", "every bucket requires name")
			return
		}
		bucket.ReplicationProviderIDs = normalizeProviderIDs(bucket.ReplicationProviderIDs)
		bucket.ReplicationEnabled = len(bucket.ReplicationProviderIDs) > 0
		if err := h.svc.Store.UpsertBucket(r.Context(), bucket); err != nil {
			writeProblem(w, http.StatusBadRequest, "bucket-apply-failed", err.Error())
			return
		}
	}
	for _, hookRequest := range request.Hooks {
		if err := h.svc.UpsertHookFromAdmin(r.Context(), hookRequest.toDomain()); err != nil {
			writeProblem(w, http.StatusBadRequest, "hook-apply-failed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers_applied": len(request.Providers), "buckets_applied": len(request.Buckets), "hooks_applied": len(request.Hooks)})
}

func (h *Handler) openapi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	paths := map[string]any{}
	for path, operations := range map[string][]string{
		"/admin/api/providers":          {"get", "post"},
		"/admin/api/buckets":            {"get", "post"},
		"/admin/api/hooks":              {"get", "post"},
		"/admin/api/objects":            {"get", "delete"},
		"/admin/api/migrations":         {"get", "post"},
		"/admin/api/inventory-jobs":     {"get", "post"},
		"/admin/api/repair-jobs":        {"get", "post"},
		"/admin/api/access-credentials": {"get", "post"},
		"/admin/api/trash":              {"get"},
		"/admin/api/placement-plan":     {"get"},
		"/admin/api/cost-optimizations": {"get"},
		"/admin/api/repair":             {"post"},
		"/admin/api/declarative/apply":  {"post"},
	} {
		pathItem := map[string]any{}
		for _, operation := range operations {
			pathItem[operation] = map[string]any{"responses": map[string]any{"200": map[string]any{"description": "Success"}, "400": map[string]any{"description": "Invalid request"}, "401": map[string]any{"description": "Authentication required"}}}
		}
		paths[path] = pathItem
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "BucketMux Admin API", "version": "1.0.0", "description": "Production administration API for providers, buckets, migrations, identity, protection and repair."},
		"paths":      paths,
		"components": map[string]any{"securitySchemes": map[string]any{"basicAuth": map[string]any{"type": "http", "scheme": "basic"}, "oidc": map[string]any{"type": "openIdConnect", "openIdConnectUrl": h.svc.Config.Admin.OIDC.IssuerURL + "/.well-known/openid-configuration"}}},
		"security":   []any{map[string]any{"basicAuth": []string{}}},
	})
}
