package gateway

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/router"
	"github.com/gnurub/bucketmux/internal/store"
)

type Handler struct {
	svc  *app.Service
	auth *Authenticator
}

func NewHandler(svc *app.Service) *Handler {
	return &Handler{svc: svc, auth: NewAuthenticator(svc.Config.S3)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setS3CORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.auth.Authorize(r) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "invalid or missing S3 credentials")
		return
	}
	if r.URL.Path == "/" {
		if r.Method == http.MethodGet {
			h.listBuckets(w, r)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "root only supports ListBuckets")
		return
	}
	bucket, key, ok := splitS3Path(r.URL.Path)
	if !ok {
		writeS3Error(w, http.StatusBadRequest, "InvalidURI", "expected path-style S3 URI /{bucket}/{key}")
		return
	}
	if key == "" {
		h.handleBucketOperation(w, r, bucket)
		return
	}
	switch r.Method {
	case http.MethodPost:
		if key != "" && hasQueryFlag(r, "uploads") {
			h.createMultipartUpload(w, r, bucket, key)
			return
		}
		if key != "" && r.URL.Query().Get("uploadId") != "" {
			h.completeMultipartUpload(w, r, bucket, key)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported POST operation")
	case http.MethodPut:
		if r.URL.Query().Get("uploadId") != "" || r.URL.Query().Get("partNumber") != "" {
			h.uploadPart(w, r, bucket, key)
			return
		}
		h.putObject(w, r, bucket, key)
	case http.MethodGet:
		if key == "" || r.URL.Query().Get("list-type") == "2" {
			h.listObjects(w, r, bucket)
			return
		}
		if r.URL.Query().Get("uploadId") != "" {
			h.listParts(w, r, bucket, key)
			return
		}
		h.getObject(w, r, bucket, key)
	case http.MethodHead:
		h.headObject(w, r, bucket, key)
	case http.MethodDelete:
		if r.URL.Query().Get("uploadId") != "" {
			h.abortMultipartUpload(w, r, bucket, key)
			return
		}
		h.deleteObject(w, r, bucket, key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method is not supported by Core S3 MVP")
	}
}

func (h *Handler) handleBucketOperation(w http.ResponseWriter, r *http.Request, bucket string) {
	switch r.Method {
	case http.MethodPut:
		if err := h.svc.Store.UpsertBucket(r.Context(), domain.Bucket{Name: bucket}); err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		if _, err := h.svc.Store.GetBucket(r.Context(), bucket); err != nil {
			writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if hasQueryFlag(r, "location") {
			h.getBucketLocation(w, r, bucket)
			return
		}
		h.listObjects(w, r, bucket)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "bucket operation is not supported")
	}
}

func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.svc.Store.ListBuckets(r.Context())
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	resp := listAllMyBucketsResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, bucket := range buckets {
		resp.Buckets.Bucket = append(resp.Buckets.Bucket, bucketEntry{Name: bucket.Name, CreationDate: bucket.CreatedAt.Format(time.RFC3339)})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(resp)
}

func (h *Handler) getBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := h.svc.Store.GetBucket(r.Context(), bucket); err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(locationConstraint{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Value: h.svc.Config.S3.Region})
}

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidObjectName", "object key is required")
		return
	}
	input := domain.PutObjectInput{Bucket: bucket, Key: key, Size: r.ContentLength, ContentType: r.Header.Get("Content-Type")}
	obj, err := h.svc.PutObject(r.Context(), input, r.Body)
	if err != nil {
		if errors.Is(err, router.ErrNoProviderAvailable) {
			writeS3Error(w, http.StatusInsufficientStorage, "InsufficientStorage", "no configured provider has enough capacity")
			return
		}
		writeS3Error(w, http.StatusBadGateway, "ProviderError", err.Error())
		return
	}
	w.Header().Set("ETag", obj.ETag)
	w.Header().Set("X-S3LS-Provider-Account", obj.ProviderAccountID)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	body, obj, err := h.svc.GetObject(r.Context(), bucket, key)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	defer body.Close()
	setObjectHeaders(w, obj)
	if r.Header.Get("Range") != "" {
		h.writeRangeObject(w, r, body, obj)
		return
	}
	_, _ = io.Copy(w, body)
}

func (h *Handler) writeRangeObject(w http.ResponseWriter, r *http.Request, body io.Reader, obj domain.ObjectRecord) {
	start, end, ok := parseSingleRange(r.Header.Get("Range"), obj.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
		writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "requested range is not satisfiable")
		return
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			writeS3Error(w, http.StatusBadGateway, "ProviderError", err.Error())
			return
		}
	}
	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.CopyN(w, body, length)
}

func (h *Handler) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj, err := h.svc.HeadObject(r.Context(), bucket, key)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	setObjectHeaders(w, obj)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := h.svc.DeleteObject(r.Context(), bucket, key); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("start-after")
	objects, err := h.svc.ListObjectsAfter(r.Context(), bucket, prefix, startAfter, limit)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	resp := listBucketResult{
		XMLName:     xml.Name{Local: "ListBucketResult"},
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucket,
		Prefix:      prefix,
		StartAfter:  startAfter,
		KeyCount:    len(objects),
		MaxKeys:     limitOrDefault(limit),
		IsTruncated: false,
	}
	for _, obj := range objects {
		resp.Contents = append(resp.Contents, objectEntry{Key: obj.Key, LastModified: obj.UpdatedAt.Format(time.RFC3339), ETag: obj.ETag, Size: obj.Size, StorageClass: "STANDARD", Owner: owner{ID: "bucketmux", DisplayName: "bucketmux"}})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(resp)
}

func (h *Handler) writeObjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "object not found")
		return
	}
	writeS3Error(w, http.StatusBadGateway, "ProviderError", err.Error())
}

func hasQueryFlag(r *http.Request, name string) bool {
	_, ok := r.URL.Query()[name]
	return ok
}

func splitS3Path(rawPath string) (bucket, key string, ok bool) {
	trimmed := strings.TrimPrefix(rawPath, "/")
	if trimmed == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	bucket = parts[0]
	if bucket == "" || strings.HasPrefix(bucket, "admin") {
		return "", "", false
	}
	if len(parts) == 2 {
		key = parts[1]
	}
	return bucket, key, true
}

func parseSingleRange(header string, size int64) (int64, int64, bool) {
	if size < 0 || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	startRaw, endRaw, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if startRaw == "" {
		suffix, err := strconv.ParseInt(endRaw, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, size > 0
	}
	start, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if endRaw != "" {
		parsedEnd, err := strconv.ParseInt(endRaw, 10, 64)
		if err != nil || parsedEnd < start {
			return 0, 0, false
		}
		if parsedEnd < end {
			end = parsedEnd
		}
	}
	return start, end, true
}

func setObjectHeaders(w http.ResponseWriter, obj domain.ObjectRecord) {
	if obj.ContentType != "" {
		w.Header().Set("Content-Type", obj.ContentType)
	}
	if obj.ETag != "" {
		w.Header().Set("ETag", obj.ETag)
	}
	if obj.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.Size))
	}
	w.Header().Set("X-S3LS-Provider-Account", obj.ProviderAccountID)
	w.Header().Set("X-S3LS-Replica-Status", obj.ReplicaStatus)
}

func setS3CORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET,PUT,POST,DELETE,HEAD,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,Content-MD5,X-Amz-Date,X-Amz-Content-Sha256,X-Amz-Security-Token,X-Amz-Acl,X-S3LS-Access-Key,X-S3LS-Secret-Key")
	w.Header().Set("Access-Control-Expose-Headers", "ETag,Location,Content-Length,Content-Range,X-S3LS-Provider-Account,X-S3LS-Replica-Status")
	w.Header().Set("Access-Control-Max-Age", "3000")
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(errorResponse{Code: code, Message: message})
}

func limitOrDefault(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 1000
	}
	return limit
}

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Buckets buckets  `xml:"Buckets"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type buckets struct {
	Bucket []bucketEntry `xml:"Bucket"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

type errorResponse struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

type listBucketResult struct {
	XMLName     xml.Name      `xml:"ListBucketResult"`
	Xmlns       string        `xml:"xmlns,attr"`
	Name        string        `xml:"Name"`
	Prefix      string        `xml:"Prefix"`
	StartAfter  string        `xml:"StartAfter,omitempty"`
	KeyCount    int           `xml:"KeyCount"`
	MaxKeys     int           `xml:"MaxKeys"`
	IsTruncated bool          `xml:"IsTruncated"`
	Contents    []objectEntry `xml:"Contents"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        owner  `xml:"Owner"`
}
