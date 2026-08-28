package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
)

// configureTursoVectors creates a native Turso vector sidecar while retaining
// values_blob as the portable representation used for API reads and migrations.
// Existing SQLite files therefore open in-place and are backfilled without
// rewriting or discarding their original embeddings.
func (s *Store) configureTursoVectors(ctx context.Context) error {
	if s.dialect != dialectTurso {
		return nil
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS object_embedding_turso_vectors (
  embedding_id TEXT PRIMARY KEY,
  profile_hash TEXT NOT NULL,
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  dimensions INTEGER NOT NULL,
  metric TEXT NOT NULL,
  embedding BLOB NOT NULL,
  FOREIGN KEY (embedding_id) REFERENCES object_embeddings(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_object_embedding_turso_vectors_profile ON object_embedding_turso_vectors(profile_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_object_embedding_turso_vectors_object ON object_embedding_turso_vectors(bucket, key)`,
	}
	for _, statement := range statements {
		if _, err := s.exec(ctx, statement); err != nil {
			return fmt.Errorf("configure turso vector storage: %w", err)
		}
	}
	s.vectorBackend = "turso-native"
	return s.backfillTursoVectors(ctx)
}

func (s *Store) backfillTursoVectors(ctx context.Context) error {
	for {
		rows, err := s.query(ctx, `
SELECT e.id, e.bucket, e.key, e.kind, e.model, e.model_version, e.metric, e.dimensions, e.values_blob
FROM object_embeddings e
LEFT JOIN object_embedding_turso_vectors v ON v.embedding_id = e.id
WHERE v.embedding_id IS NULL
ORDER BY e.id
LIMIT 1000`)
		if err != nil {
			return fmt.Errorf("query turso vector backfill: %w", err)
		}
		type pendingVector struct {
			id, bucket, key string
			profile         vectorProfile
			blob            []byte
		}
		var pending []pendingVector
		for rows.Next() {
			var item pendingVector
			if err := rows.Scan(&item.id, &item.bucket, &item.key, &item.profile.Kind, &item.profile.Model, &item.profile.ModelVersion, &item.profile.Metric, &item.profile.Dimensions, &item.blob); err != nil {
				_ = rows.Close()
				return err
			}
			pending = append(pending, item)
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if len(pending) == 0 {
			return nil
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, item := range pending {
			values, err := decodeFloat32Vector(item.blob, item.profile.Dimensions)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("decode turso vector backfill %s: %w", item.id, err)
			}
			if err := insertTursoVector(ctx, tx, item.id, item.bucket, item.key, item.profile, values); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit turso vector backfill: %w", err)
		}
	}
}

func insertTursoVector(ctx context.Context, tx *sql.Tx, id, bucket, key string, profile vectorProfile, values []float32) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO object_embedding_turso_vectors (embedding_id, profile_hash, bucket, key, dimensions, metric, embedding)
VALUES (?, ?, ?, ?, ?, ?, vector32(?))
ON CONFLICT (embedding_id) DO UPDATE SET
  profile_hash=excluded.profile_hash, bucket=excluded.bucket, key=excluded.key,
  dimensions=excluded.dimensions, metric=excluded.metric, embedding=excluded.embedding`,
		id, profile.hash(), bucket, key, profile.Dimensions, profile.Metric, formatPGVector(values))
	if err != nil {
		return fmt.Errorf("insert turso vector embedding %s: %w", id, err)
	}
	return nil
}

func (s *Store) searchTursoVectors(ctx context.Context, query domain.EmbeddingSearchQuery) ([]domain.EmbeddingSearchResult, error) {
	distanceFunction := map[string]string{
		"cosine": "vector_distance_cos",
		"dot":    "vector_distance_dot",
		"l2":     "vector_distance_l2",
	}[query.Metric]
	if distanceFunction == "" {
		return nil, fmt.Errorf("unsupported turso vector metric %q", query.Metric)
	}
	where := []string{"e.model = ?", "e.metric = ?", "e.dimensions = ?"}
	args := []any{query.Model, query.Metric, len(query.Values)}
	if query.ModelVersion != "" {
		where = append(where, "e.model_version = ?")
		args = append(args, query.ModelVersion)
	}
	if query.Kind != "" {
		where = append(where, "e.kind = ?")
		args = append(args, query.Kind)
	}
	if query.Bucket != "" {
		where = append(where, "e.bucket = ?")
		args = append(args, query.Bucket)
	}

	vectorText := formatPGVector(query.Values)
	distanceExpression := distanceFunction + `(embedding, query_embedding)`
	scoreExpression := `-(` + distanceExpression + `)`
	if query.Metric == "cosine" {
		scoreExpression = `1 - (` + distanceExpression + `)`
	}
	args = append(args, query.MaxCandidates, vectorText)
	minScoreClause := ""
	if query.MinScore != nil {
		minScoreClause = ` WHERE ` + scoreExpression + ` >= ?`
		args = append(args, *query.MinScore)
	}
	args = append(args, query.Limit)

	statement := `WITH candidates AS (
SELECT ` + strings.TrimPrefix(embeddingSummarySelect, "SELECT ") + `, v.embedding
FROM object_embeddings e
JOIN object_embedding_turso_vectors v ON v.embedding_id = e.id
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY e.id
LIMIT ?
), query_vector AS (
SELECT vector32(?) AS query_embedding
)
SELECT id, bucket, key, source_checksum, source_updated_at, plugin_id, kind, model, model_version, metric, dimensions, ordinal, metadata_json, created_at, updated_at,
       ` + scoreExpression + ` AS score
FROM candidates CROSS JOIN query_vector` + minScoreClause + `
ORDER BY score DESC, id
LIMIT ?`
	rows, err := s.query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search turso vector embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var results []domain.EmbeddingSearchResult
	for rows.Next() {
		embedding, score, err := scanEmbeddingSummaryWithScore(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, domain.EmbeddingSearchResult{Embedding: embedding, Score: score})
	}
	return results, rows.Err()
}
