package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
)

func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if bucket == "" || key == "" {
		writeProblem(w, http.StatusBadRequest, "invalid-embedding-query", "bucket and key are required")
		return
	}
	embeddings, err := h.svc.ListObjectEmbeddings(r.Context(), bucket, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not-found", "Object not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "embedding-list-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, embeddings)
}

func (h *Handler) searchEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	var query domain.EmbeddingSearchQuery
	if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-json", err.Error())
		return
	}
	results, err := h.svc.SearchObjectEmbeddings(r.Context(), query)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid-embedding-query", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (h *Handler) embeddingCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "method is not allowed")
		return
	}
	capabilities, err := h.svc.Store.VectorSearchCapabilities(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "embedding-capabilities-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, capabilities)
}
