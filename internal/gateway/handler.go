package gateway

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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
	return &Handler{svc: svc, auth: NewAuthenticatorWithResolver(svc.Config.S3, svc)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setS3CORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	bucket, key, ok := splitS3Path(r.URL.Path)
	if ok && key == "" && r.Method == http.MethodPost && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		h.postObject(w, r, bucket)
		return
	}
	if _, ok := h.auth.AuthorizeAction(r, s3PermissionForRequest(r, bucket, key), bucket, key); !ok {
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
		if hasQueryFlag(r, "tagging") {
			h.putObjectTagging(w, r, bucket, key)
			return
		}
		if hasQueryFlag(r, "retention") {
			h.putObjectRetention(w, r, bucket, key)
			return
		}
		if hasQueryFlag(r, "legal-hold") {
			h.putObjectLegalHold(w, r, bucket, key)
			return
		}
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			h.copyObject(w, r, bucket, key)
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
		if hasQueryFlag(r, "tagging") {
			h.getObjectTagging(w, r, bucket, key)
			return
		}
		if hasQueryFlag(r, "retention") {
			h.getObjectRetention(w, r, bucket, key)
			return
		}
		if hasQueryFlag(r, "legal-hold") {
			h.getObjectLegalHold(w, r, bucket, key)
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
		if hasQueryFlag(r, "tagging") {
			h.deleteObjectTagging(w, r, bucket, key)
			return
		}
		h.deleteObject(w, r, bucket, key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method is not supported by Core S3 MVP")
	}
}

func s3PermissionForRequest(r *http.Request, bucket, key string) string {
	if bucket == "" {
		return "s3:ListAllMyBuckets"
	}
	if key == "" {
		switch r.Method {
		case http.MethodPut:
			return "s3:CreateBucket"
		case http.MethodGet, http.MethodHead:
			return "s3:ListBucket"
		case http.MethodPost:
			return "s3:DeleteObject"
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return "s3:GetObject"
	case http.MethodPut, http.MethodPost:
		return "s3:PutObject"
	case http.MethodDelete:
		return "s3:DeleteObject"
	default:
		return "s3:*"
	}
}

func (h *Handler) handleBucketOperation(w http.ResponseWriter, r *http.Request, bucket string) {
	switch r.Method {
	case http.MethodPut:
		if hasQueryFlag(r, "notification") {
			h.putBucketNotification(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "versioning") {
			h.putBucketVersioning(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "object-lock") {
			h.putBucketObjectLock(w, r, bucket)
			return
		}
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
		if hasQueryFlag(r, "notification") {
			h.getBucketNotification(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "versioning") {
			h.getBucketVersioning(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "object-lock") {
			h.getBucketObjectLock(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "versions") {
			h.listObjectVersions(w, r, bucket)
			return
		}
		if hasQueryFlag(r, "location") {
			h.getBucketLocation(w, r, bucket)
			return
		}
		h.listObjects(w, r, bucket)
	case http.MethodPost:
		if hasQueryFlag(r, "delete") {
			h.deleteObjects(w, r, bucket)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "bucket POST operation is not supported")
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
	if r.ContentLength > h.svc.Config.Server.MaxUploadBytes {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "object exceeds the configured upload limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.svc.Config.Server.MaxUploadBytes)
	input := domain.PutObjectInput{Bucket: bucket, Key: key, Size: r.ContentLength, ContentType: r.Header.Get("Content-Type"), Metadata: objectMetadataFromHeaders(r.Header), Tags: parseTaggingHeader(r.Header.Get("X-Amz-Tagging")), RetentionMode: strings.ToUpper(r.Header.Get("X-Amz-Object-Lock-Mode")), LegalHold: strings.EqualFold(r.Header.Get("X-Amz-Object-Lock-Legal-Hold"), "ON")}
	if retainUntil := r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"); retainUntil != "" {
		input.RetainUntil, _ = time.Parse(time.RFC3339, retainUntil)
	}
	obj, err := h.svc.PutObject(r.Context(), input, r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.Is(err, app.ErrUploadTooLarge) || errors.As(err, &tooLarge) {
			writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", "object exceeds the configured upload limit")
			return
		}
		if errors.Is(err, router.ErrNoProviderAvailable) {
			writeS3Error(w, http.StatusInsufficientStorage, "InsufficientStorage", "no configured provider has enough capacity")
			return
		}
		writeS3Error(w, http.StatusBadGateway, "ProviderError", err.Error())
		return
	}
	w.Header().Set("ETag", obj.ETag)
	w.Header().Set("X-S3LS-Provider-Account", obj.ProviderAccountID)
	if obj.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", obj.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var body io.ReadCloser
	var obj domain.ObjectRecord
	var err error
	if versionID := r.URL.Query().Get("versionId"); versionID != "" {
		body, obj, err = h.svc.GetObjectVersion(r.Context(), bucket, key, versionID)
	} else {
		body, obj, err = h.svc.GetObject(r.Context(), bucket, key)
	}
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	defer func() { _ = body.Close() }()
	if status, stop := evaluateObjectConditions(r, obj); stop {
		w.WriteHeader(status)
		return
	}
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
	var obj domain.ObjectRecord
	var err error
	if versionID := r.URL.Query().Get("versionId"); versionID != "" {
		obj, err = h.svc.HeadObjectVersion(r.Context(), bucket, key, versionID)
	} else {
		obj, err = h.svc.HeadObject(r.Context(), bucket, key)
	}
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	if status, stop := evaluateObjectConditions(r, obj); stop {
		w.WriteHeader(status)
		return
	}
	setObjectHeaders(w, obj)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	result, err := h.svc.DeleteObjectWithOptions(r.Context(), bucket, key, app.DeleteObjectOptions{VersionID: r.URL.Query().Get("versionId"), BypassGovernance: strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true")})
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	if result.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", result.VersionID)
	}
	if result.DeleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	limit = limitOrDefault(limit)
	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("start-after")
	continuationToken := r.URL.Query().Get("continuation-token")
	if continuationToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(continuationToken)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "continuation-token is invalid")
			return
		}
		startAfter = string(decoded)
	}
	objects, err := h.svc.ListObjectsAfter(r.Context(), bucket, prefix, startAfter, limit+1)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	truncated := len(objects) > limit
	if truncated {
		objects = objects[:limit]
	}
	delimiter := r.URL.Query().Get("delimiter")
	resp := listBucketResult{
		XMLName:           xml.Name{Local: "ListBucketResult"},
		Xmlns:             "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:              bucket,
		Prefix:            prefix,
		StartAfter:        startAfter,
		ContinuationToken: continuationToken,
		Delimiter:         delimiter,
		MaxKeys:           limit,
		IsTruncated:       truncated,
	}
	commonPrefixes := map[string]bool{}
	for _, obj := range objects {
		if delimiter != "" {
			rest := strings.TrimPrefix(obj.Key, prefix)
			if index := strings.Index(rest, delimiter); index >= 0 {
				commonPrefixes[prefix+rest[:index+len(delimiter)]] = true
				continue
			}
		}
		resp.Contents = append(resp.Contents, objectEntry{Key: obj.Key, LastModified: obj.UpdatedAt.Format(time.RFC3339), ETag: obj.ETag, Size: obj.Size, StorageClass: "STANDARD", Owner: owner{ID: "bucketmux", DisplayName: "bucketmux"}})
	}
	for commonPrefix := range commonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, commonPrefixEntry{Prefix: commonPrefix})
	}
	slices.SortFunc(resp.CommonPrefixes, func(a, b commonPrefixEntry) int { return strings.Compare(a.Prefix, b.Prefix) })
	resp.KeyCount = len(resp.Contents) + len(resp.CommonPrefixes)
	if truncated && len(objects) > 0 {
		resp.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(objects[len(objects)-1].Key))
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(resp)
}

func (h *Handler) writeObjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, app.ErrUploadTooLarge) {
		writeS3Error(w, http.StatusRequestEntityTooLarge, "EntityTooLarge", err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "object not found")
		return
	}
	if errors.Is(err, app.ErrObjectLocked) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", err.Error())
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
	if obj.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", obj.VersionID)
	}
	for name, value := range obj.Metadata {
		w.Header().Set("X-Amz-Meta-"+name, value)
	}
	writeObjectLockHeaders(w, obj)
}

func setS3CORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET,PUT,POST,DELETE,HEAD,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,Content-MD5,If-Match,If-None-Match,If-Modified-Since,If-Unmodified-Since,X-Amz-Date,X-Amz-Content-Sha256,X-Amz-Security-Token,X-Amz-Acl,X-Amz-Copy-Source,X-Amz-Metadata-Directive,X-Amz-Tagging,X-S3LS-Access-Key,X-S3LS-Secret-Key")
	w.Header().Set("Access-Control-Expose-Headers", "ETag,Location,Content-Length,Content-Range,X-Amz-Version-Id,X-S3LS-Provider-Account,X-S3LS-Replica-Status")
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
	XMLName               xml.Name            `xml:"ListBucketResult"`
	Xmlns                 string              `xml:"xmlns,attr"`
	Name                  string              `xml:"Name"`
	Prefix                string              `xml:"Prefix"`
	StartAfter            string              `xml:"StartAfter,omitempty"`
	ContinuationToken     string              `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string              `xml:"NextContinuationToken,omitempty"`
	Delimiter             string              `xml:"Delimiter,omitempty"`
	KeyCount              int                 `xml:"KeyCount"`
	MaxKeys               int                 `xml:"MaxKeys"`
	IsTruncated           bool                `xml:"IsTruncated"`
	Contents              []objectEntry       `xml:"Contents"`
	CommonPrefixes        []commonPrefixEntry `xml:"CommonPrefixes,omitempty"`
}

type commonPrefixEntry struct {
	Prefix string `xml:"Prefix"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        owner  `xml:"Owner"`
}
