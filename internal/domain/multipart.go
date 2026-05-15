package domain

import "time"

type MultipartUpload struct {
	UploadID    string
	Bucket      string
	Key         string
	ContentType string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type MultipartPart struct {
	UploadID       string
	PartNumber     int
	Path           string
	Size           int64
	ETag           string
	ChecksumSHA256 string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
