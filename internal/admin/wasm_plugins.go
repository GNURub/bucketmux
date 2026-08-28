package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
)

type wasmPluginRequest struct {
	ID               string                            `json:"id"`
	Name             string                            `json:"name"`
	Description      string                            `json:"description"`
	ABIVersion       string                            `json:"abi_version"`
	ModuleBase64     string                            `json:"module_base64"`
	Events           []string                          `json:"events"`
	BucketPattern    string                            `json:"bucket_pattern"`
	KeyPrefix        string                            `json:"key_prefix"`
	KeySuffix        string                            `json:"key_suffix"`
	ContentTypes     []string                          `json:"content_types"`
	Config           map[string]string                 `json:"config"`
	OperationPolicy  *domain.WASMPluginOperationPolicy `json:"operation_policy"`
	Enabled          bool                              `json:"enabled"`
	TimeoutMillis    int                               `json:"timeout_millis"`
	MemoryLimitBytes int64                             `json:"memory_limit_bytes"`
	MaxInputBytes    int64                             `json:"max_input_bytes"`
	MaxOutputBytes   int64                             `json:"max_output_bytes"`
	MaxAttempts      int                               `json:"max_attempts"`
}

func (request wasmPluginRequest) toDomain() domain.WASMPlugin {
	var operationPolicy domain.WASMPluginOperationPolicy
	if request.OperationPolicy != nil {
		operationPolicy = *request.OperationPolicy
	}
	return domain.WASMPlugin{
		ID: strings.TrimSpace(request.ID), Name: strings.TrimSpace(request.Name), Description: strings.TrimSpace(request.Description),
		ABIVersion: request.ABIVersion, ModuleBase64: strings.TrimSpace(request.ModuleBase64), Events: request.Events,
		BucketPattern: request.BucketPattern, KeyPrefix: request.KeyPrefix, KeySuffix: request.KeySuffix,
		ContentTypes: request.ContentTypes, Config: request.Config, Enabled: request.Enabled,
		OperationPolicy: operationPolicy,
		TimeoutMillis:   request.TimeoutMillis, MemoryLimitBytes: request.MemoryLimitBytes,
		MaxInputBytes: request.MaxInputBytes, MaxOutputBytes: request.MaxOutputBytes, MaxAttempts: request.MaxAttempts,
	}
}

func (h *Handler) wasmPlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		plugins, err := h.svc.Store.ListWASMPlugins(r.Context(), false)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, plugins)
	case http.MethodPost:
		var request wasmPluginRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
			return
		}
		plugin := request.toDomain()
		if plugin.ModuleBase64 == "" && plugin.ID != "" {
			existing, err := h.svc.Store.GetWASMPlugin(r.Context(), plugin.ID)
			if err == nil {
				plugin.ModuleBase64 = existing.ModuleBase64
				if request.OperationPolicy == nil {
					plugin.OperationPolicy = existing.OperationPolicy
				}
			}
		}
		if err := h.svc.UpsertWASMPlugin(r.Context(), plugin); err != nil {
			writeProblem(w, http.StatusBadRequest, "wasm-plugin-invalid", err.Error())
			return
		}
		stored, err := h.svc.Store.GetWASMPlugin(r.Context(), plugin.ID)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, stored)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
	}
}

func (h *Handler) validateWASMPlugin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	var request wasmPluginRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
		return
	}
	if err := h.svc.ValidateWASMPlugin(r.Context(), request.toDomain()); err != nil {
		writeProblem(w, http.StatusBadRequest, "wasm-plugin-invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "abi_version": domain.WASMPluginABIV1})
}

func (h *Handler) wasmPluginByID(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	if _, err := h.svc.Store.GetWASMPlugin(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not-found", "WASM plugin not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	if err := h.svc.Store.DeleteWASMPlugin(r.Context(), id); err != nil {
		writeProblem(w, http.StatusInternalServerError, "wasm-plugin-delete-failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) wasmPluginJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	jobs, err := h.svc.Store.ListWASMPluginJobs(r.Context(), 100)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "store-error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}
