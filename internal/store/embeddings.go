package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const (
	defaultEmbeddingCandidates = 10_000
	maximumEmbeddingCandidates = 100_000
	maximumEmbeddingResults    = 100
	maximumEmbeddingsPerPlugin = 128
	maximumEmbeddingDimensions = 4096
)

var ErrObjectGenerationSuperseded = errors.New("object generation was superseded")

// ReplaceObjectEmbeddings atomically replaces every embedding produced by one
// plugin for one immutable object generation. Retries therefore cannot create
// duplicates or leave a half-written set behind.
func (s *Store) ReplaceObjectEmbeddings(ctx context.Context, object domain.ObjectRecord, pluginID string, embeddings []domain.WASMPluginEmbedding) error {
	if object.Bucket == "" || object.Key == "" || pluginID == "" {
		return errors.New("bucket, key and plugin id are required")
	}
	if len(embeddings) > maximumEmbeddingsPerPlugin {
		return fmt.Errorf("embedding count %d exceeds maximum %d", len(embeddings), maximumEmbeddingsPerPlugin)
	}
	for i, embedding := range embeddings {
		if err := validateStoredEmbedding(embedding); err != nil {
			return fmt.Errorf("embedding %d: %w", i, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// This conditional no-op write both verifies the immutable generation and
	// serializes it against concurrent overwrites before replacing vectors.
	// A check performed only by the caller would leave a TOCTOU window.
	generation, err := tx.ExecContext(ctx, s.rebind(`
UPDATE objects SET updated_at = updated_at
WHERE bucket = ? AND key = ? AND checksum_sha256 = ? AND updated_at = ?`),
		object.Bucket, object.Key, object.ChecksumSHA256, optionalTimeString(object.UpdatedAt))
	if err != nil {
		return fmt.Errorf("lock embedding source generation: %w", err)
	}
	matched, err := generation.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect embedding source generation: %w", err)
	}
	if matched != 1 {
		return ErrObjectGenerationSuperseded
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM object_embeddings WHERE bucket = ? AND key = ? AND plugin_id = ?`), object.Bucket, object.Key, pluginID); err != nil {
		return fmt.Errorf("delete previous embeddings: %w", err)
	}
	now := time.Now().UTC()
	profiles := make(map[string]vectorProfile)
	for ordinal, embedding := range embeddings {
		metadata, err := json.Marshal(embedding.Metadata)
		if err != nil {
			return fmt.Errorf("encode embedding metadata: %w", err)
		}
		values, err := encodeFloat32Vector(embedding.Values)
		if err != nil {
			return err
		}
		identity := strings.Join([]string{object.Bucket, object.Key, object.ChecksumSHA256, object.UpdatedAt.UTC().Format(time.RFC3339Nano), pluginID, embedding.Kind, embedding.Model, embedding.ModelVersion, fmt.Sprint(ordinal)}, "\x00")
		digest := sha256.Sum256([]byte(identity))
		id := "emb-" + hex.EncodeToString(digest[:16])
		_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO object_embeddings (id, bucket, key, source_checksum, source_updated_at, plugin_id, kind, model, model_version, metric, dimensions, ordinal, values_blob, metadata_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, object.Bucket, object.Key, object.ChecksumSHA256, optionalTimeString(object.UpdatedAt), pluginID,
			embedding.Kind, embedding.Model, embedding.ModelVersion, embedding.Metric, embedding.Dimensions, ordinal,
			values, string(metadata), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("insert embedding %d: %w", ordinal, err)
		}
		profile := vectorProfile{Kind: embedding.Kind, Model: embedding.Model, ModelVersion: embedding.ModelVersion, Metric: embedding.Metric, Dimensions: embedding.Dimensions}
		switch s.vectorBackend {
		case "pgvector":
			if err := insertPGVector(ctx, tx, id, object.Bucket, object.Key, profile, embedding.Values); err != nil {
				return err
			}
			profiles[profile.hash()] = profile
		case "turso-native":
			if err := insertTursoVector(ctx, tx, id, object.Bucket, object.Key, profile, embedding.Values); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, profile := range profiles {
		if err := s.ensurePGVectorIndex(ctx, profile); err != nil {
			return err
		}
	}
	return nil
}

func validateStoredEmbedding(embedding domain.WASMPluginEmbedding) error {
	if strings.TrimSpace(embedding.Model) == "" || strings.TrimSpace(embedding.ModelVersion) == "" || strings.TrimSpace(embedding.Kind) == "" {
		return errors.New("kind, model and model version are required")
	}
	if embedding.Metric != "cosine" && embedding.Metric != "dot" && embedding.Metric != "l2" {
		return fmt.Errorf("unsupported metric %q", embedding.Metric)
	}
	if embedding.Dimensions <= 0 || embedding.Dimensions > maximumEmbeddingDimensions || embedding.Dimensions != len(embedding.Values) {
		return fmt.Errorf("dimensions %d do not match %d values or exceed maximum %d", embedding.Dimensions, len(embedding.Values), maximumEmbeddingDimensions)
	}
	if embedding.Metric == "cosine" && vectorSquaredNorm(embedding.Values) == 0 {
		return errors.New("cosine embeddings cannot be a zero vector")
	}
	_, err := encodeFloat32Vector(embedding.Values)
	return err
}

func (s *Store) DeleteObjectEmbeddings(ctx context.Context, bucket, key string) error {
	_, err := s.exec(ctx, `DELETE FROM object_embeddings WHERE bucket = ? AND key = ?`, bucket, key)
	return err
}

func (s *Store) ListObjectEmbeddings(ctx context.Context, bucket, key string, includeValues bool) ([]domain.ObjectEmbedding, error) {
	rows, err := s.query(ctx, embeddingSelect+` WHERE bucket = ? AND key = ? ORDER BY plugin_id, kind, model, model_version, ordinal`, bucket, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.ObjectEmbedding
	for rows.Next() {
		embedding, err := scanEmbedding(rows, includeValues)
		if err != nil {
			return nil, err
		}
		result = append(result, embedding)
	}
	return result, rows.Err()
}

// SearchObjectEmbeddings keeps one API across Turso's native exact functions,
// pgvector ANN, and the portable exact fallback.
func (s *Store) SearchObjectEmbeddings(ctx context.Context, query domain.EmbeddingSearchQuery) ([]domain.EmbeddingSearchResult, error) {
	if err := normalizeEmbeddingSearchQuery(&query); err != nil {
		return nil, err
	}
	if s.vectorBackend == "pgvector" {
		return s.searchPGVector(ctx, query)
	}
	if s.vectorBackend == "turso-native" {
		return s.searchTursoVectors(ctx, query)
	}
	where := []string{"model = ?", "metric = ?", "dimensions = ?"}
	args := []any{query.Model, query.Metric, len(query.Values)}
	if query.ModelVersion != "" {
		where = append(where, "model_version = ?")
		args = append(args, query.ModelVersion)
	}
	if query.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, query.Kind)
	}
	if query.Bucket != "" {
		where = append(where, "bucket = ?")
		args = append(args, query.Bucket)
	}
	args = append(args, query.MaxCandidates)
	rows, err := s.query(ctx, embeddingSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	results := make([]domain.EmbeddingSearchResult, 0, query.Limit)
	queryNorm := vectorSquaredNorm(query.Values)
	for rows.Next() {
		embedding, err := scanEmbedding(rows, true)
		if err != nil {
			return nil, err
		}
		score, ok := embeddingScoreWithQueryNorm(query.Metric, query.Values, embedding.Values, queryNorm)
		if !ok || query.MinScore != nil && score < *query.MinScore {
			continue
		}
		embedding.Values = nil
		candidate := domain.EmbeddingSearchResult{Embedding: embedding, Score: score}
		if len(results) < query.Limit {
			results = append(results, candidate)
			continue
		}
		worst := 0
		for i := 1; i < len(results); i++ {
			if embeddingResultBetter(results[worst], results[i]) {
				worst = i
			}
		}
		if embeddingResultBetter(candidate, results[worst]) {
			results[worst] = candidate
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Embedding.ID < results[j].Embedding.ID
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > query.Limit {
		results = results[:query.Limit]
	}
	return results, nil
}

const embeddingSelect = `SELECT id, bucket, key, source_checksum, source_updated_at, plugin_id, kind, model, model_version, metric, dimensions, ordinal, values_blob, metadata_json, created_at, updated_at FROM object_embeddings`

func scanEmbedding(row scanner, includeValues bool) (domain.ObjectEmbedding, error) {
	var embedding domain.ObjectEmbedding
	var values []byte
	var metadataJSON, sourceUpdatedAt, createdAt, updatedAt string
	if err := row.Scan(&embedding.ID, &embedding.Bucket, &embedding.Key, &embedding.SourceChecksum, &sourceUpdatedAt,
		&embedding.PluginID, &embedding.Kind, &embedding.Model, &embedding.ModelVersion, &embedding.Metric,
		&embedding.Dimensions, &embedding.Ordinal, &values, &metadataJSON, &createdAt, &updatedAt); err != nil {
		return embedding, err
	}
	decoded, err := decodeFloat32Vector(values, embedding.Dimensions)
	if err != nil {
		return embedding, fmt.Errorf("decode embedding %s: %w", embedding.ID, err)
	}
	if includeValues {
		embedding.Values = decoded
	}
	if err := json.Unmarshal([]byte(metadataJSON), &embedding.Metadata); err != nil {
		return embedding, fmt.Errorf("decode embedding metadata %s: %w", embedding.ID, err)
	}
	if embedding.Metadata == nil {
		embedding.Metadata = map[string]string{}
	}
	embedding.SourceUpdatedAt = parseOptionalTime(sourceUpdatedAt)
	embedding.CreatedAt = parseOptionalTime(createdAt)
	embedding.UpdatedAt = parseOptionalTime(updatedAt)
	return embedding, nil
}

func encodeFloat32Vector(values []float32) ([]byte, error) {
	data := make([]byte, len(values)*4)
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding value %d is not finite", i)
		}
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(value))
	}
	return data, nil
}

func decodeFloat32Vector(data []byte, dimensions int) ([]float32, error) {
	if dimensions <= 0 || len(data) != dimensions*4 {
		return nil, fmt.Errorf("blob has %d bytes for %d dimensions", len(data), dimensions)
	}
	values := make([]float32, dimensions)
	for i := range values {
		values[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
		if math.IsNaN(float64(values[i])) || math.IsInf(float64(values[i]), 0) {
			return nil, fmt.Errorf("value %d is not finite", i)
		}
	}
	return values, nil
}

func normalizeEmbeddingSearchQuery(query *domain.EmbeddingSearchQuery) error {
	query.Model = strings.TrimSpace(query.Model)
	query.ModelVersion = strings.TrimSpace(query.ModelVersion)
	query.Kind = strings.TrimSpace(query.Kind)
	query.Bucket = strings.TrimSpace(query.Bucket)
	query.Metric = strings.ToLower(strings.TrimSpace(query.Metric))
	if query.Metric == "" {
		query.Metric = "cosine"
	}
	if query.Model == "" || len(query.Values) == 0 {
		return errors.New("model and a non-empty values vector are required")
	}
	if len(query.Values) > maximumEmbeddingDimensions {
		return fmt.Errorf("query has %d dimensions; maximum is %d", len(query.Values), maximumEmbeddingDimensions)
	}
	if query.Metric != "cosine" && query.Metric != "dot" && query.Metric != "l2" {
		return fmt.Errorf("unsupported embedding metric %q", query.Metric)
	}
	if _, err := encodeFloat32Vector(query.Values); err != nil {
		return err
	}
	if query.Metric == "cosine" && vectorSquaredNorm(query.Values) == 0 {
		return errors.New("cosine query cannot be a zero vector")
	}
	if query.Limit <= 0 {
		query.Limit = 10
	}
	if query.Limit > maximumEmbeddingResults {
		query.Limit = maximumEmbeddingResults
	}
	if query.MaxCandidates <= 0 {
		query.MaxCandidates = defaultEmbeddingCandidates
	}
	if query.MaxCandidates > maximumEmbeddingCandidates {
		query.MaxCandidates = maximumEmbeddingCandidates
	}
	return nil
}

func embeddingScore(metric string, query, candidate []float32) (float64, bool) {
	return embeddingScoreWithQueryNorm(metric, query, candidate, vectorSquaredNorm(query))
}

func embeddingScoreWithQueryNorm(metric string, query, candidate []float32, queryNorm float64) (float64, bool) {
	if len(query) == 0 || len(query) != len(candidate) {
		return 0, false
	}
	var dot, candidateNorm, squaredDistance float64
	for i := range query {
		q, c := float64(query[i]), float64(candidate[i])
		dot += q * c
		candidateNorm += c * c
		difference := q - c
		squaredDistance += difference * difference
	}
	switch metric {
	case "dot":
		return dot, true
	case "l2":
		return -math.Sqrt(squaredDistance), true
	case "cosine":
		if queryNorm == 0 || candidateNorm == 0 {
			return 0, false
		}
		return dot / (math.Sqrt(queryNorm) * math.Sqrt(candidateNorm)), true
	default:
		return 0, false
	}
}

func vectorSquaredNorm(values []float32) float64 {
	var norm float64
	for _, value := range values {
		norm += float64(value) * float64(value)
	}
	return norm
}

func embeddingResultBetter(left, right domain.EmbeddingSearchResult) bool {
	if left.Score == right.Score {
		return left.Embedding.ID < right.Embedding.ID
	}
	return left.Score > right.Score
}

func optionalTimeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
