package gateway

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
)

func objectMetadataFromHeaders(header http.Header) map[string]string {
	metadata := map[string]string{}
	for name, values := range header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-meta-") {
			continue
		}
		metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
	}
	return metadata
}

func parseTaggingHeader(raw string) map[string]string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return map[string]string{}
	}
	tags := make(map[string]string, len(values))
	for key, value := range values {
		if len(value) > 0 {
			tags[key] = value[0]
		}
	}
	return tags
}

func evaluateObjectConditions(r *http.Request, object domain.ObjectRecord) (int, bool) {
	etag := strings.TrimSpace(object.ETag)
	if match := strings.TrimSpace(r.Header.Get("If-Match")); match != "" && match != "*" && !etagListContains(match, etag) {
		return http.StatusPreconditionFailed, true
	}
	if noneMatch := strings.TrimSpace(r.Header.Get("If-None-Match")); noneMatch != "" && (noneMatch == "*" || etagListContains(noneMatch, etag)) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return http.StatusNotModified, true
		}
		return http.StatusPreconditionFailed, true
	}
	if modifiedSince, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !object.UpdatedAt.IsZero() && !object.UpdatedAt.After(modifiedSince) {
		return http.StatusNotModified, true
	}
	if unmodifiedSince, err := http.ParseTime(r.Header.Get("If-Unmodified-Since")); err == nil && !object.UpdatedAt.IsZero() && object.UpdatedAt.After(unmodifiedSince) {
		return http.StatusPreconditionFailed, true
	}
	return 0, false
}

func etagListContains(list, etag string) bool {
	for _, candidate := range strings.Split(list, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, targetBucket, targetKey string) {
	source := strings.TrimSpace(r.Header.Get("X-Amz-Copy-Source"))
	decoded, err := url.PathUnescape(strings.TrimPrefix(source, "/"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source is invalid")
		return
	}
	sourceBucket, sourceKey, ok := splitS3Path("/" + decoded)
	if !ok || sourceKey == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source must identify an object")
		return
	}
	principal, authenticated := h.auth.Authenticate(r)
	if !authenticated || !principal.Allows("s3:GetObject", sourceBucket, sourceKey) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "credential cannot read the copy source")
		return
	}
	body, sourceObject, err := h.svc.GetObject(r.Context(), sourceBucket, sourceKey)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	defer func() { _ = body.Close() }()
	metadata := sourceObject.Metadata
	contentType := sourceObject.ContentType
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		metadata = objectMetadataFromHeaders(r.Header)
		if replacement := r.Header.Get("Content-Type"); replacement != "" {
			contentType = replacement
		}
	}
	tags := sourceObject.Tags
	if strings.EqualFold(r.Header.Get("X-Amz-Tagging-Directive"), "REPLACE") {
		tags = parseTaggingHeader(r.Header.Get("X-Amz-Tagging"))
	}
	object, err := h.svc.PutObject(r.Context(), domain.PutObjectInput{Bucket: targetBucket, Key: targetKey, Size: sourceObject.Size, ContentType: contentType, Metadata: metadata, Tags: tags}, body)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(copyObjectResult{LastModified: object.UpdatedAt.UTC().Format(time.RFC3339), ETag: object.ETag})
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type deleteObjectsRequest struct {
	Objects []objectIdentifier `xml:"Object"`
	Quiet   bool               `xml:"Quiet"`
}

type objectIdentifier struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

type deleteObjectsResult struct {
	XMLName xml.Name           `xml:"DeleteResult"`
	Xmlns   string             `xml:"xmlns,attr"`
	Deleted []objectIdentifier `xml:"Deleted,omitempty"`
	Errors  []deleteError      `xml:"Error,omitempty"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (h *Handler) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var request deleteObjectsRequest
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	if len(request.Objects) > 1000 {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "DeleteObjects accepts at most 1000 keys")
		return
	}
	result := deleteObjectsResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	principal, _ := h.auth.Authenticate(r)
	for _, identifier := range request.Objects {
		if identifier.Key == "" {
			result.Errors = append(result.Errors, deleteError{Code: "InvalidArgument", Message: "object key is required"})
			continue
		}
		if !principal.Allows("s3:DeleteObject", bucket, identifier.Key) {
			result.Errors = append(result.Errors, deleteError{Key: identifier.Key, Code: "AccessDenied", Message: "credential cannot delete this object"})
			continue
		}
		err := h.svc.DeleteObject(r.Context(), bucket, identifier.Key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			result.Errors = append(result.Errors, deleteError{Key: identifier.Key, Code: "InternalError", Message: err.Error()})
			continue
		}
		if !request.Quiet {
			result.Deleted = append(result.Deleted, identifier)
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	TagSet  []tag    `xml:"TagSet>Tag"`
}

type tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (h *Handler) getObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	object, err := h.svc.GetProtectedObject(r.Context(), bucket, key, r.URL.Query().Get("versionId"))
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	response := tagging{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for key, value := range object.Tags {
		response.TagSet = append(response.TagSet, tag{Key: key, Value: value})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}

func (h *Handler) putObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var request tagging
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	if len(request.TagSet) > 10 {
		writeS3Error(w, http.StatusBadRequest, "BadRequest", "an object can have at most 10 tags")
		return
	}
	tags := map[string]string{}
	for _, tag := range request.TagSet {
		if tag.Key == "" || len(tag.Key) > 128 || len(tag.Value) > 256 {
			writeS3Error(w, http.StatusBadRequest, "InvalidTag", "tag keys must be 1-128 characters and values at most 256 characters")
			return
		}
		if _, duplicate := tags[tag.Key]; duplicate {
			writeS3Error(w, http.StatusBadRequest, "InvalidTag", fmt.Sprintf("duplicate tag %q", tag.Key))
			return
		}
		tags[tag.Key] = tag.Value
	}
	if err := h.svc.UpdateObjectTags(r.Context(), bucket, key, r.URL.Query().Get("versionId"), tags); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := h.svc.UpdateObjectTags(r.Context(), bucket, key, r.URL.Query().Get("versionId"), map[string]string{}); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type postObjectResult struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (h *Handler) postObject(w http.ResponseWriter, r *http.Request, bucket string) {
	limit := h.svc.Config.Server.MaxUploadBytes + (2 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedPOSTRequest", err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "file form field is required")
		return
	}
	defer func() { _ = file.Close() }()
	fields := map[string]string{"bucket": bucket}
	for name, values := range r.MultipartForm.Value {
		if len(values) > 0 {
			fields[strings.ToLower(name)] = values[0]
		}
	}
	key := strings.ReplaceAll(fields["key"], "${filename}", header.Filename)
	key = strings.TrimLeft(key, "/")
	if key == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "key form field is required")
		return
	}
	fields["key"] = key
	if _, err := h.auth.AuthorizePostPolicy(r.Context(), fields, header.Size); err != nil {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", err.Error())
		return
	}
	metadata := map[string]string{}
	for name, value := range fields {
		if strings.HasPrefix(name, "x-amz-meta-") {
			metadata[strings.TrimPrefix(name, "x-amz-meta-")] = value
		}
	}
	contentType := fields["content-type"]
	if contentType == "" {
		contentType = header.Header.Get("Content-Type")
	}
	object, err := h.svc.PutObject(r.Context(), domain.PutObjectInput{Bucket: bucket, Key: key, Size: header.Size, ContentType: contentType, Metadata: metadata, Tags: parseTaggingHeader(fields["x-amz-tagging"])}, file)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	status := http.StatusNoContent
	if parsed, err := strconv.Atoi(fields["success_action_status"]); err == nil && (parsed == http.StatusOK || parsed == http.StatusCreated || parsed == http.StatusNoContent) {
		status = parsed
	}
	if status == http.StatusCreated {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_ = xml.NewEncoder(w).Encode(postObjectResult{Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: object.ETag})
		return
	}
	w.Header().Set("ETag", object.ETag)
	w.WriteHeader(status)
}
