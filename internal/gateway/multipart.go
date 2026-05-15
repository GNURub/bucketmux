package gateway

import (
	"encoding/xml"
	"net/http"
	"strconv"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (h *Handler) createMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	upload, err := h.svc.CreateMultipartUpload(r.Context(), bucket, key, r.Header.Get("Content-Type"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "CreateMultipartUploadFailed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(initiateMultipartUploadResult{Bucket: bucket, Key: key, UploadID: upload.UploadID})
}

func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if uploadID == "" || err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "uploadId and numeric partNumber are required")
		return
	}
	part, err := h.svc.UploadPart(r.Context(), uploadID, partNumber, r.Body)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.Header().Set("ETag", part.ETag)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	var request completeMultipartUploadRequest
	_ = xml.NewDecoder(r.Body).Decode(&request)
	requestedParts := make([]int, 0, len(request.Parts))
	for _, part := range request.Parts {
		requestedParts = append(requestedParts, part.PartNumber)
	}
	obj, err := h.svc.CompleteMultipartUpload(r.Context(), uploadID, requestedParts)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(completeMultipartUploadResult{Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: obj.ETag})
}

func (h *Handler) abortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := h.svc.AbortMultipartUpload(r.Context(), r.URL.Query().Get("uploadId")); err != nil {
		h.writeObjectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listParts(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	parts, err := h.svc.ListMultipartParts(r.Context(), uploadID)
	if err != nil {
		h.writeObjectError(w, err)
		return
	}
	result := listPartsResult{Bucket: bucket, Key: key, UploadID: uploadID, IsTruncated: false}
	for _, part := range parts {
		result.Parts = append(result.Parts, listPartEntry{PartNumber: part.PartNumber, ETag: part.ETag, Size: part.Size})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUploadRequest struct {
	Parts []completePartRequest `xml:"Part"`
}

type completePartRequest struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type listPartsResult struct {
	XMLName     xml.Name        `xml:"ListPartsResult"`
	Bucket      string          `xml:"Bucket"`
	Key         string          `xml:"Key"`
	UploadID    string          `xml:"UploadId"`
	IsTruncated bool            `xml:"IsTruncated"`
	Parts       []listPartEntry `xml:"Part"`
}

type listPartEntry struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

var _ = domain.MultipartPart{}
