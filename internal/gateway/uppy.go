package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
)

type UppyHandler struct {
	svc  *app.Service
	auth *Authenticator
}

func NewUppyHandler(svc *app.Service) *UppyHandler {
	return &UppyHandler{svc: svc, auth: NewAuthenticator(svc.Config.S3)}
}

func (h *UppyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setS3CORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.auth.Authorize(r) {
		writeJSONError(w, http.StatusForbidden, "invalid or missing BucketMux credentials")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/uppy/s3")
	switch path {
	case "/sign":
		h.signS3Request(w, r)
	case "/params":
		h.getUploadParameters(w, r)
	case "/multipart/create":
		h.createMultipart(w, r)
	case "/multipart/sign":
		h.signMultipartPart(w, r)
	case "/multipart/list":
		h.listMultipartParts(w, r)
	case "/multipart/complete":
		h.completeMultipart(w, r)
	case "/multipart/abort":
		h.abortMultipart(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown Uppy S3 helper endpoint")
	}
}

// signS3Request implements the @uppy/aws-s3 v6 signRequest contract. Uppy v6
// performs the S3 operations itself and asks the application for one presigned
// URL per request, including multipart creation, listing, completion and abort.
// The same endpoint also signs direct object reads and deletes for browser clients
// that use fetch without Uppy.
func (h *UppyHandler) signS3Request(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppySignRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	bucket := defaultString(req.Bucket, "images")
	key := strings.Trim(strings.TrimSpace(req.Key), "/")
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	query := url.Values{}
	switch method {
	case http.MethodPut:
		if req.UploadID == "" && req.PartNumber != 0 {
			writeJSONError(w, http.StatusBadRequest, "uploadId is required when partNumber is set")
			return
		}
		if req.UploadID != "" {
			if req.PartNumber <= 0 {
				writeJSONError(w, http.StatusBadRequest, "partNumber is required for multipart PUT")
				return
			}
			query.Set("uploadId", req.UploadID)
			query.Set("partNumber", strconv.Itoa(req.PartNumber))
		}
	case http.MethodPost:
		if req.PartNumber != 0 {
			writeJSONError(w, http.StatusBadRequest, "partNumber is not valid for POST")
			return
		}
		if req.UploadID == "" {
			query.Set("uploads", "")
		} else {
			query.Set("uploadId", req.UploadID)
		}
	case http.MethodGet:
		if req.PartNumber != 0 {
			writeJSONError(w, http.StatusBadRequest, "partNumber is not valid for GET")
			return
		}
		if req.UploadID != "" {
			query.Set("uploadId", req.UploadID)
		}
	case http.MethodDelete:
		if req.PartNumber != 0 {
			writeJSONError(w, http.StatusBadRequest, "partNumber is not valid for DELETE")
			return
		}
		if req.UploadID != "" {
			query.Set("uploadId", req.UploadID)
		}
	default:
		writeJSONError(w, http.StatusBadRequest, "method must be PUT, POST, GET or DELETE")
		return
	}

	if len(query) == 0 {
		query = nil
	}
	presignedURL, ok := h.presignForRequest(r, method, bucket, key, query, expiresDuration(req.ExpiresIn))
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "could not presign S3 request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": presignedURL})
}

func (h *UppyHandler) getUploadParameters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppyUploadParamsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	bucket := defaultString(req.Bucket, "images")
	key := strings.Trim(strings.TrimSpace(req.Key), "/")
	if key == "" {
		key = strings.Trim(strings.TrimSpace(req.Filename), "/")
	}
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "key or filename is required")
		return
	}
	presignedURL, ok := h.presignForRequest(r, http.MethodPut, bucket, key, nil, expiresDuration(req.ExpiresIn))
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "could not presign upload URL")
		return
	}
	headers := map[string]string{}
	if req.ContentType != "" {
		headers["content-type"] = req.ContentType
	}
	writeJSON(w, http.StatusOK, map[string]any{"method": "PUT", "url": presignedURL, "fields": map[string]string{}, "headers": headers})
}

func (h *UppyHandler) createMultipart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppyMultipartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	bucket := defaultString(req.Bucket, "images")
	key := strings.Trim(strings.TrimSpace(req.Key), "/")
	if key == "" {
		key = strings.Trim(strings.TrimSpace(req.Filename), "/")
	}
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "key or filename is required")
		return
	}
	upload, err := h.svc.CreateMultipartUpload(r.Context(), bucket, key, req.ContentType)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"uploadId": upload.UploadID, "key": upload.Key, "bucket": upload.Bucket})
}

func (h *UppyHandler) signMultipartPart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppyMultipartPartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UploadID == "" || req.Key == "" || req.PartNumber <= 0 {
		writeJSONError(w, http.StatusBadRequest, "uploadId, key and partNumber are required")
		return
	}
	bucket := defaultString(req.Bucket, "images")
	query := url.Values{"uploadId": {req.UploadID}, "partNumber": {strconv.Itoa(req.PartNumber)}}
	presignedURL, ok := h.presignForRequest(r, http.MethodPut, bucket, req.Key, query, expiresDuration(req.ExpiresIn))
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "could not presign part URL")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": presignedURL, "headers": map[string]string{}})
}

func (h *UppyHandler) listMultipartParts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" && r.Method == http.MethodPost {
		var req uppyMultipartPartRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		uploadID = req.UploadID
	}
	parts, err := h.svc.ListMultipartParts(r.Context(), uploadID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		out = append(out, map[string]any{"PartNumber": part.PartNumber, "Size": part.Size, "ETag": part.ETag})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *UppyHandler) completeMultipart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppyCompleteMultipartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	requested := make([]int, 0, len(req.Parts))
	for _, part := range req.Parts {
		requested = append(requested, part.PartNumber)
	}
	obj, err := h.svc.CompleteMultipartUpload(r.Context(), req.UploadID, requested)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"location": "/" + obj.Bucket + "/" + obj.Key, "bucket": obj.Bucket, "key": obj.Key})
}

func (h *UppyHandler) abortMultipart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req uppyMultipartPartRequest
	if r.Method == http.MethodPost {
		if !decodeJSON(w, r, &req) {
			return
		}
	} else {
		req.UploadID = r.URL.Query().Get("uploadId")
	}
	if err := h.svc.AbortMultipartUpload(r.Context(), req.UploadID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *UppyHandler) presignForRequest(r *http.Request, method, bucket, key string, query url.Values, expires time.Duration) (string, bool) {
	base := publicBaseURL(r)
	target, err := url.Parse(base + "/" + url.PathEscape(bucket) + "/" + escapeS3Key(key))
	if err != nil {
		return "", false
	}
	if query != nil {
		target.RawQuery = query.Encode()
	}
	return h.auth.PresignURL(method, *target, expires)
}

func publicBaseURL(r *http.Request) string {
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

func escapeS3Key(key string) string {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func expiresDuration(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}

type uppyUploadParamsRequest struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	ExpiresIn   int    `json:"expiresIn"`
}

type uppySignRequest struct {
	Method     string `json:"method"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	UploadID   string `json:"uploadId"`
	PartNumber int    `json:"partNumber"`
	ExpiresIn  int    `json:"expiresIn"`
}

type uppyMultipartRequest struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
}

type uppyMultipartPartRequest struct {
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	UploadID   string `json:"uploadId"`
	PartNumber int    `json:"partNumber"`
	ExpiresIn  int    `json:"expiresIn"`
}

type uppyCompleteMultipartRequest struct {
	Bucket   string     `json:"bucket"`
	Key      string     `json:"key"`
	UploadID string     `json:"uploadId"`
	Parts    []uppyPart `json:"parts"`
}

type uppyPart struct {
	PartNumber int    `json:"PartNumber"`
	ETag       string `json:"ETag"`
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
