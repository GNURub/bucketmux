package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
)

var ErrEmbeddingSourceSuperseded = errors.New("embedding source generation was superseded")

type EmbeddingImport struct {
	Bucket          string
	Key             string
	ProducerID      string
	SourceChecksum  string
	SourceUpdatedAt time.Time
	Embeddings      []domain.WASMPluginEmbedding
}

// ImportObjectEmbeddings persists vectors produced by an authenticated
// external worker only when the object generation it processed is still the
// current generation. Values remain write-only through the admin API.
func (s *Service) ImportObjectEmbeddings(ctx context.Context, input EmbeddingImport) ([]domain.ObjectEmbedding, error) {
	input.Bucket = strings.TrimSpace(input.Bucket)
	input.Key = strings.TrimSpace(input.Key)
	input.ProducerID = strings.TrimSpace(input.ProducerID)
	input.SourceChecksum = strings.TrimSpace(input.SourceChecksum)
	if input.Bucket == "" || input.Key == "" || input.ProducerID == "" || input.SourceChecksum == "" || input.SourceUpdatedAt.IsZero() {
		return nil, errors.New("bucket, key, producer_id, source_checksum and source_updated_at are required")
	}
	if len(input.ProducerID) > 128 || strings.ContainsAny(input.ProducerID, "\x00\r\n") {
		return nil, errors.New("producer_id is invalid")
	}
	object, err := s.Store.GetObject(ctx, input.Bucket, input.Key)
	if err != nil {
		return nil, err
	}
	if object.ChecksumSHA256 != input.SourceChecksum || !object.UpdatedAt.Equal(input.SourceUpdatedAt) {
		return nil, ErrEmbeddingSourceSuperseded
	}
	if err := s.Store.ReplaceObjectEmbeddings(ctx, object, input.ProducerID, input.Embeddings); err != nil {
		if errors.Is(err, store.ErrObjectGenerationSuperseded) {
			return nil, ErrEmbeddingSourceSuperseded
		}
		return nil, fmt.Errorf("store imported embeddings: %w", err)
	}
	return s.Store.ListObjectEmbeddings(ctx, input.Bucket, input.Key, false)
}
