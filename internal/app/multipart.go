package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Service) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (domain.MultipartUpload, error) {
	if _, err := s.ensureBucket(ctx, bucket); err != nil {
		return domain.MultipartUpload{}, err
	}
	uploadID, err := randomUploadID()
	if err != nil {
		return domain.MultipartUpload{}, err
	}
	upload := domain.MultipartUpload{UploadID: uploadID, Bucket: bucket, Key: key, ContentType: contentType}
	if err := s.Store.CreateMultipartUpload(ctx, upload); err != nil {
		return domain.MultipartUpload{}, err
	}
	return upload, nil
}

func (s *Service) UploadPart(ctx context.Context, uploadID string, partNumber int, body io.Reader) (domain.MultipartPart, error) {
	if partNumber < 1 || partNumber > 10000 {
		return domain.MultipartPart{}, fmt.Errorf("partNumber must be between 1 and 10000")
	}
	if _, err := s.Store.GetMultipartUpload(ctx, uploadID); err != nil {
		return domain.MultipartPart{}, err
	}
	path := filepath.Join(s.Config.Server.MultipartStagingDir, uploadID, fmt.Sprintf("%05d.part", partNumber))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return domain.MultipartPart{}, fmt.Errorf("create multipart dir: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".bucketmux-part-*.tmp")
	if err != nil {
		return domain.MultipartPart{}, fmt.Errorf("create multipart part: %w", err)
	}
	tempPath := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(file, io.TeeReader(io.LimitReader(body, s.Config.Server.MaxMultipartPartBytes+1), hash))
	if copyErr != nil {
		return domain.MultipartPart{}, fmt.Errorf("write multipart part: %w", copyErr)
	}
	if written > s.Config.Server.MaxMultipartPartBytes {
		return domain.MultipartPart{}, fmt.Errorf("%w: multipart part maximum is %d bytes", ErrUploadTooLarge, s.Config.Server.MaxMultipartPartBytes)
	}
	if err := file.Sync(); err != nil {
		return domain.MultipartPart{}, fmt.Errorf("sync multipart part: %w", err)
	}
	if err := file.Close(); err != nil {
		return domain.MultipartPart{}, fmt.Errorf("close multipart part: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return domain.MultipartPart{}, fmt.Errorf("commit multipart part: %w", err)
	}
	committed = true
	checksum := hex.EncodeToString(hash.Sum(nil))
	part := domain.MultipartPart{UploadID: uploadID, PartNumber: partNumber, Path: path, Size: written, ETag: `"` + checksum + `"`, ChecksumSHA256: checksum}
	if err := s.Store.UpsertMultipartPart(ctx, part); err != nil {
		return domain.MultipartPart{}, err
	}
	return part, nil
}

func (s *Service) CompleteMultipartUpload(ctx context.Context, uploadID string, requestedParts []int) (domain.ObjectRecord, error) {
	upload, err := s.Store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	parts, err := s.Store.ListMultipartParts(ctx, uploadID)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	ordered, totalSize, err := orderMultipartParts(parts, requestedParts)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	if totalSize > s.Config.Server.MaxUploadBytes {
		return domain.ObjectRecord{}, fmt.Errorf("%w: maximum is %d bytes", ErrUploadTooLarge, s.Config.Server.MaxUploadBytes)
	}
	reader, closeFn, err := openMultipartReader(ordered)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	defer closeFn()
	obj, err := s.PutObject(ctx, domain.PutObjectInput{Bucket: upload.Bucket, Key: upload.Key, Size: totalSize, ContentType: upload.ContentType}, reader)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	_ = s.cleanupMultipart(ctx, uploadID, ordered)
	return obj, nil
}

func (s *Service) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	parts, _ := s.Store.ListMultipartParts(ctx, uploadID)
	return s.cleanupMultipart(ctx, uploadID, parts)
}

func (s *Service) ListMultipartParts(ctx context.Context, uploadID string) ([]domain.MultipartPart, error) {
	if _, err := s.Store.GetMultipartUpload(ctx, uploadID); err != nil {
		return nil, err
	}
	return s.Store.ListMultipartParts(ctx, uploadID)
}

func (s *Service) cleanupMultipart(ctx context.Context, uploadID string, parts []domain.MultipartPart) error {
	for _, part := range parts {
		_ = os.Remove(part.Path)
	}
	_ = os.RemoveAll(filepath.Join(s.Config.Server.MultipartStagingDir, uploadID))
	return s.Store.DeleteMultipartUpload(ctx, uploadID)
}

func orderMultipartParts(parts []domain.MultipartPart, requested []int) ([]domain.MultipartPart, int64, error) {
	byNumber := map[int]domain.MultipartPart{}
	for _, part := range parts {
		byNumber[part.PartNumber] = part
	}
	if len(requested) == 0 {
		requested = make([]int, 0, len(parts))
		for _, part := range parts {
			requested = append(requested, part.PartNumber)
		}
		sort.Ints(requested)
	}
	ordered := make([]domain.MultipartPart, 0, len(requested))
	var total int64
	previous := 0
	for _, partNumber := range requested {
		if partNumber <= previous {
			return nil, 0, fmt.Errorf("multipart parts must be in strictly ascending order")
		}
		part, ok := byNumber[partNumber]
		if !ok {
			return nil, 0, fmt.Errorf("multipart part %d was not uploaded", partNumber)
		}
		ordered = append(ordered, part)
		total += part.Size
		previous = partNumber
	}
	if len(ordered) == 0 {
		return nil, 0, fmt.Errorf("multipart upload has no parts")
	}
	return ordered, total, nil
}

func openMultipartReader(parts []domain.MultipartPart) (io.Reader, func(), error) {
	readers := make([]io.Reader, 0, len(parts))
	files := make([]*os.File, 0, len(parts))
	closeFn := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	for _, part := range parts {
		file, err := os.Open(part.Path)
		if err != nil {
			closeFn()
			return nil, closeFn, fmt.Errorf("open multipart part %d: %w", part.PartNumber, err)
		}
		files = append(files, file)
		readers = append(readers, file)
	}
	return io.MultiReader(readers...), closeFn, nil
}

func randomUploadID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
