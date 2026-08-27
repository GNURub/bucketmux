package gateway

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

func (h *Handler) getBucketVersioning(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.svc.Store.GetBucket(r.Context(), bucketName)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	status := "Suspended"
	if bucket.VersioningEnabled {
		status = "Enabled"
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(versioningConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Status: status})
}

func (h *Handler) putBucketVersioning(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.svc.Store.GetBucket(r.Context(), bucketName)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	var request versioningConfiguration
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	if request.Status != "Enabled" && request.Status != "Suspended" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Status must be Enabled or Suspended")
		return
	}
	bucket.VersioningEnabled = request.Status == "Enabled"
	if err := h.svc.Store.UpsertBucket(r.Context(), bucket); err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

type objectLockConfiguration struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	Xmlns             string   `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled,omitempty"`
	Rule              *struct {
		DefaultRetention struct {
			Mode  string `xml:"Mode"`
			Days  int    `xml:"Days,omitempty"`
			Years int    `xml:"Years,omitempty"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule,omitempty"`
}

func (h *Handler) getBucketObjectLock(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.svc.Store.GetBucket(r.Context(), bucketName)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	response := objectLockConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	if bucket.ObjectLockEnabled {
		response.ObjectLockEnabled = "Enabled"
	}
	if bucket.DefaultRetentionDays > 0 {
		response.Rule = &struct {
			DefaultRetention struct {
				Mode  string `xml:"Mode"`
				Days  int    `xml:"Days,omitempty"`
				Years int    `xml:"Years,omitempty"`
			} `xml:"DefaultRetention"`
		}{}
		response.Rule.DefaultRetention.Mode = bucket.DefaultRetentionMode
		response.Rule.DefaultRetention.Days = bucket.DefaultRetentionDays
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}

func (h *Handler) putBucketObjectLock(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.svc.Store.GetBucket(r.Context(), bucketName)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		return
	}
	var request objectLockConfiguration
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	if request.ObjectLockEnabled != "Enabled" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "ObjectLockEnabled must be Enabled")
		return
	}
	bucket.ObjectLockEnabled = true
	bucket.VersioningEnabled = true
	if request.Rule != nil {
		mode := strings.ToUpper(request.Rule.DefaultRetention.Mode)
		if mode != "GOVERNANCE" && mode != "COMPLIANCE" {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "retention Mode must be GOVERNANCE or COMPLIANCE")
			return
		}
		days := request.Rule.DefaultRetention.Days + request.Rule.DefaultRetention.Years*365
		if days <= 0 {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "retention Days or Years must be positive")
			return
		}
		bucket.DefaultRetentionMode = mode
		bucket.DefaultRetentionDays = days
	}
	if err := h.svc.Store.UpsertBucket(r.Context(), bucket); err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

type retentionConfiguration struct {
	XMLName         xml.Name `xml:"Retention"`
	Xmlns           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode"`
	RetainUntilDate string   `xml:"RetainUntilDate"`
}

func (h *Handler) getObjectRetention(w http.ResponseWriter, r *http.Request, bucket, key string) {
	object, err := h.svc.GetProtectedObject(r.Context(), bucket, key, r.URL.Query().Get("versionId"))
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(retentionConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Mode: object.RetentionMode, RetainUntilDate: object.RetainUntil.UTC().Format(time.RFC3339)})
}

func (h *Handler) putObjectRetention(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var request retentionConfiguration
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	mode := strings.ToUpper(request.Mode)
	retainUntil, err := time.Parse(time.RFC3339, request.RetainUntilDate)
	if err != nil || (mode != "GOVERNANCE" && mode != "COMPLIANCE") || !retainUntil.After(time.Now().UTC()) {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "retention requires a future RFC3339 date and GOVERNANCE or COMPLIANCE mode")
		return
	}
	versionID := r.URL.Query().Get("versionId")
	object, err := h.svc.GetProtectedObject(r.Context(), bucket, key, versionID)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	object.RetentionMode = mode
	object.RetainUntil = retainUntil
	if err := h.svc.UpdateObjectProtection(r.Context(), object, strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true")); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type legalHoldConfiguration struct {
	XMLName xml.Name `xml:"LegalHold"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

func (h *Handler) getObjectLegalHold(w http.ResponseWriter, r *http.Request, bucket, key string) {
	object, err := h.svc.GetProtectedObject(r.Context(), bucket, key, r.URL.Query().Get("versionId"))
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	status := "OFF"
	if object.LegalHold {
		status = "ON"
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(legalHoldConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Status: status})
}

func (h *Handler) putObjectLegalHold(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var request legalHoldConfiguration
	if err := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || (request.Status != "ON" && request.Status != "OFF") {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "legal hold Status must be ON or OFF")
		return
	}
	versionID := r.URL.Query().Get("versionId")
	object, err := h.svc.GetProtectedObject(r.Context(), bucket, key, versionID)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	object.LegalHold = request.Status == "ON"
	if err := h.svc.UpdateObjectProtection(r.Context(), object, false); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type listVersionsResult struct {
	XMLName       xml.Name            `xml:"ListVersionsResult"`
	Xmlns         string              `xml:"xmlns,attr"`
	Name          string              `xml:"Name"`
	Prefix        string              `xml:"Prefix"`
	MaxKeys       int                 `xml:"MaxKeys"`
	IsTruncated   bool                `xml:"IsTruncated"`
	Versions      []versionEntry      `xml:"Version,omitempty"`
	DeleteMarkers []deleteMarkerEntry `xml:"DeleteMarker,omitempty"`
}

type versionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type deleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
}

func (h *Handler) listObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	prefix := r.URL.Query().Get("prefix")
	versions, err := h.svc.Store.ListObjectVersions(r.Context(), bucket, prefix, limit)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	response := listVersionsResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: bucket, Prefix: prefix, MaxKeys: limit}
	latestSeen := map[string]bool{}
	for _, object := range versions {
		latest := !latestSeen[object.Key]
		latestSeen[object.Key] = true
		if object.IsDeleteMarker {
			response.DeleteMarkers = append(response.DeleteMarkers, deleteMarkerEntry{Key: object.Key, VersionID: object.VersionID, IsLatest: latest, LastModified: object.CreatedAt.UTC().Format(time.RFC3339)})
		} else {
			response.Versions = append(response.Versions, versionEntry{Key: object.Key, VersionID: object.VersionID, IsLatest: latest, LastModified: object.CreatedAt.UTC().Format(time.RFC3339), ETag: object.ETag, Size: object.Size, StorageClass: "STANDARD"})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}

func writeObjectLockHeaders(w http.ResponseWriter, object domain.ObjectRecord) {
	if object.RetentionMode != "" {
		w.Header().Set("X-Amz-Object-Lock-Mode", object.RetentionMode)
	}
	if !object.RetainUntil.IsZero() {
		w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", object.RetainUntil.UTC().Format(time.RFC3339))
	}
	if object.LegalHold {
		w.Header().Set("X-Amz-Object-Lock-Legal-Hold", "ON")
	}
}
