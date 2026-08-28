package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

const (
	pgVectorSetupLockID    int64 = 7334247568550895610
	pgVectorBackfillLockID int64 = 7334247568550895611
	maxPGVectorHNSWDim           = 2000
)

type vectorProfile struct {
	Kind, Model, ModelVersion, Metric string
	Dimensions                        int
}

func normalizeVectorConfig(cfg *config.VectorSearchConfig) {
	cfg.Backend = strings.ToLower(strings.TrimSpace(cfg.Backend))
	if cfg.Backend == "" {
		cfg.Backend = "auto"
	}
	if cfg.HNSWM == 0 {
		cfg.HNSWM = 16
	}
	if cfg.EFConstruction == 0 {
		cfg.EFConstruction = 64
	}
	if cfg.EFSearch == 0 {
		cfg.EFSearch = 100
	}
	if cfg.MaxScanTuples == 0 {
		cfg.MaxScanTuples = 20_000
	}
	if cfg.MaxProfiles == 0 {
		cfg.MaxProfiles = 64
	}
}

func (s *Store) configurePGVector(ctx context.Context) error {
	if s.dialect != dialectPostgres || s.vectorConfig.Backend == "exact" {
		s.vectorBackend = "exact"
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", pgVectorSetupLockID); err != nil {
		return fmt.Errorf("lock pgvector setup: %w", err)
	}
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&installed); err != nil {
		return fmt.Errorf("inspect pgvector extension: %w", err)
	}
	if !installed {
		if _, err := tx.ExecContext(ctx, `CREATE EXTENSION vector`); err != nil {
			if s.vectorConfig.Backend == "auto" {
				_ = tx.Rollback()
				s.vectorBackend = "exact"
				return nil
			}
			return fmt.Errorf("vector search requires the PostgreSQL vector extension: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS object_embedding_vectors (
  embedding_id TEXT PRIMARY KEY,
  profile_hash TEXT NOT NULL,
  bucket TEXT NOT NULL,
  key TEXT NOT NULL,
  dimensions INTEGER NOT NULL,
  metric TEXT NOT NULL,
  embedding vector NOT NULL,
  FOREIGN KEY (embedding_id) REFERENCES object_embeddings(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_object_embedding_vectors_profile ON object_embedding_vectors(profile_hash);
CREATE INDEX IF NOT EXISTS idx_object_embedding_vectors_object ON object_embedding_vectors(bucket, key);
`); err != nil {
		return fmt.Errorf("create pgvector embedding tables: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pgvector setup: %w", err)
	}
	s.vectorBackend = "pgvector"
	if err := s.backfillPGVectors(ctx); err != nil {
		return err
	}
	if err := s.cleanupStalePGVectorIndexes(ctx); err != nil {
		return err
	}
	return s.ensureExistingPGVectorIndexes(ctx)
}

func (s *Store) cleanupStalePGVectorIndexes(ctx context.Context) error {
	liveRows, err := s.query(ctx, `SELECT DISTINCT profile_hash FROM object_embedding_vectors`)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for liveRows.Next() {
		var hash string
		if err := liveRows.Scan(&hash); err != nil {
			_ = liveRows.Close()
			return err
		}
		if len(hash) >= 20 {
			live[hash[:20]] = true
		}
	}
	if err := liveRows.Err(); err != nil {
		_ = liveRows.Close()
		return err
	}
	_ = liveRows.Close()
	indexRows, err := s.query(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND indexname LIKE 'idx_embedding_hnsw_%'`)
	if err != nil {
		return err
	}
	var stale []string
	for indexRows.Next() {
		var name string
		if err := indexRows.Scan(&name); err != nil {
			_ = indexRows.Close()
			return err
		}
		suffix := strings.TrimPrefix(name, "idx_embedding_hnsw_")
		_, hexErr := hex.DecodeString(suffix)
		if len(suffix) == 20 && hexErr == nil && !live[suffix] {
			stale = append(stale, name)
		}
	}
	if err := indexRows.Err(); err != nil {
		_ = indexRows.Close()
		return err
	}
	_ = indexRows.Close()
	for _, name := range stale {
		if _, err := s.exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+name); err != nil {
			return fmt.Errorf("drop stale pgvector index %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) backfillPGVectors(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := acquirePGAdvisoryLock(ctx, conn, pgVectorBackfillLockID); err != nil {
		return fmt.Errorf("lock pgvector backfill: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, pgVectorBackfillLockID)
	}()
	for {
		rows, err := conn.QueryContext(ctx, `
SELECT e.id, e.bucket, e.key, e.kind, e.model, e.model_version, e.metric, e.dimensions, e.values_blob
FROM object_embeddings e
LEFT JOIN object_embedding_vectors v ON v.embedding_id = e.id
WHERE v.embedding_id IS NULL
ORDER BY e.id
LIMIT 1000`)
		if err != nil {
			return fmt.Errorf("query pgvector backfill: %w", err)
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
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, item := range pending {
			values, err := decodeFloat32Vector(item.blob, item.profile.Dimensions)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("decode pgvector backfill %s: %w", item.id, err)
			}
			if err := insertPGVector(ctx, tx, item.id, item.bucket, item.key, item.profile, values); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit pgvector backfill: %w", err)
		}
	}
}

func (s *Store) ensureExistingPGVectorIndexes(ctx context.Context) error {
	rows, err := s.query(ctx, `
SELECT DISTINCT e.kind, e.model, e.model_version, e.metric, e.dimensions
FROM object_embeddings e
JOIN object_embedding_vectors v ON v.embedding_id = e.id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var profiles []vectorProfile
	for rows.Next() {
		var profile vectorProfile
		if err := rows.Scan(&profile.Kind, &profile.Model, &profile.ModelVersion, &profile.Metric, &profile.Dimensions); err != nil {
			return err
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, profile := range profiles {
		if err := s.ensurePGVectorIndex(ctx, profile); err != nil {
			return err
		}
	}
	return nil
}

func insertPGVector(ctx context.Context, tx *sql.Tx, id, bucket, key string, profile vectorProfile, values []float32) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO object_embedding_vectors (embedding_id, profile_hash, bucket, key, dimensions, metric, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
ON CONFLICT (embedding_id) DO UPDATE SET
  profile_hash=excluded.profile_hash, bucket=excluded.bucket, key=excluded.key,
  dimensions=excluded.dimensions, metric=excluded.metric, embedding=excluded.embedding`,
		id, profile.hash(), bucket, key, profile.Dimensions, profile.Metric, formatPGVector(values))
	if err != nil {
		return fmt.Errorf("insert pgvector embedding %s: %w", id, err)
	}
	return nil
}

func (s *Store) ensurePGVectorIndex(ctx context.Context, profile vectorProfile) error {
	if s.vectorBackend != "pgvector" || profile.Dimensions <= 0 || profile.Dimensions > maxPGVectorHNSWDim {
		return nil
	}
	operatorClass := map[string]string{"cosine": "vector_cosine_ops", "dot": "vector_ip_ops", "l2": "vector_l2_ops"}[profile.Metric]
	if operatorClass == "" {
		return nil
	}
	hash := profile.hash()
	indexName := "idx_embedding_hnsw_" + hash[:20]
	lockID := profile.lockID()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := acquirePGAdvisoryLock(ctx, conn, lockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID) }()
	var exists, valid bool
	if err := conn.QueryRowContext(ctx, `
SELECT to_regclass($1) IS NOT NULL,
       COALESCE((SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass($1)), false)`, indexName).Scan(&exists, &valid); err != nil {
		return err
	}
	if exists && valid {
		return nil
	}
	if exists {
		if _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY `+indexName); err != nil {
			return fmt.Errorf("drop invalid pgvector index %s: %w", indexName, err)
		}
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname LIKE 'idx_embedding_hnsw_%'`).Scan(&count); err != nil {
		return err
	}
	if count >= s.vectorConfig.MaxProfiles {
		return fmt.Errorf("pgvector HNSW profile limit %d reached; increase store.vector_search.max_profiles", s.vectorConfig.MaxProfiles)
	}
	statement := fmt.Sprintf(`CREATE INDEX CONCURRENTLY %s ON object_embedding_vectors USING hnsw ((embedding::vector(%d)) %s) WITH (m = %d, ef_construction = %d) WHERE profile_hash = '%s'`,
		indexName, profile.Dimensions, operatorClass, s.vectorConfig.HNSWM, s.vectorConfig.EFConstruction, hash)
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create pgvector HNSW index %s: %w", indexName, err)
	}
	return nil
}

func acquirePGAdvisoryLock(ctx context.Context, conn *sql.Conn, lockID int64) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Store) searchPGVector(ctx context.Context, query domain.EmbeddingSearchQuery) ([]domain.EmbeddingSearchResult, error) {
	profile := vectorProfile{Kind: query.Kind, Model: query.Model, ModelVersion: query.ModelVersion, Metric: query.Metric, Dimensions: len(query.Values)}
	operator := map[string]string{"cosine": "<=>", "dot": "<#>", "l2": "<->"}[query.Metric]
	if operator == "" {
		return nil, fmt.Errorf("unsupported pgvector metric %q", query.Metric)
	}
	vectorExpression := "v.embedding"
	where := []string{"v.dimensions = ?", "v.metric = ?"}
	args := []any{formatPGVector(query.Values), len(query.Values), query.Metric}
	if query.Kind != "" && query.ModelVersion != "" {
		vectorExpression = fmt.Sprintf("v.embedding::vector(%d)", len(query.Values))
		where = append(where, "v.profile_hash = '"+profile.hash()+"'")
	} else {
		where = append(where, "e.model = ?")
		args = append(args, query.Model)
		if query.Kind != "" {
			where = append(where, "e.kind = ?")
			args = append(args, query.Kind)
		}
		if query.ModelVersion != "" {
			where = append(where, "e.model_version = ?")
			args = append(args, query.ModelVersion)
		}
	}
	if query.Bucket != "" {
		where = append(where, "v.bucket = ?")
		args = append(args, query.Bucket)
	}
	scoreExpression := "-(" + vectorExpression + " " + operator + " ?::vector)"
	if query.Metric == "cosine" {
		scoreExpression = "1 - (" + vectorExpression + " " + operator + " ?::vector)"
	}
	// The score parameter appears before WHERE in the SQL text.
	scoreVector := args[0]
	args = append([]any{scoreVector}, args[1:]...)
	args = append(args, formatPGVector(query.Values), query.Limit)
	statement := embeddingSummarySelect + `, ` + scoreExpression + ` AS score
FROM object_embedding_vectors v
JOIN object_embeddings e ON e.id = v.embedding_id
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY ` + vectorExpression + ` ` + operator + ` ?::vector
LIMIT ?`
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, s.vectorConfig.EFSearch)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'strict_order'`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL hnsw.max_scan_tuples = %d`, s.vectorConfig.MaxScanTuples)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, s.rebind(statement), args...)
	if err != nil {
		return nil, fmt.Errorf("search pgvector embeddings: %w", err)
	}
	var results []domain.EmbeddingSearchResult
	for rows.Next() {
		embedding, score, err := scanEmbeddingSummaryWithScore(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if query.MinScore == nil || score >= *query.MinScore {
			results = append(results, domain.EmbeddingSearchResult{Embedding: embedding, Score: score})
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		return nil, rowsErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Store) VectorSearchCapabilities(ctx context.Context) (domain.VectorSearchCapabilities, error) {
	capabilities := domain.VectorSearchCapabilities{Backend: "exact", Engine: string(s.dialect)}
	if s.vectorBackend == "turso-native" {
		capabilities.Backend = "turso-native-exact"
		capabilities.Engine = "turso"
		return capabilities, nil
	}
	if s.vectorBackend != "pgvector" {
		return capabilities, nil
	}
	capabilities.Backend = "pgvector"
	capabilities.Engine = "postgresql-pgvector"
	capabilities.ANN = true
	capabilities.MaxProfiles = s.vectorConfig.MaxProfiles
	capabilities.EFSearch = s.vectorConfig.EFSearch
	capabilities.MaxScanTuples = s.vectorConfig.MaxScanTuples
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname LIKE 'idx_embedding_hnsw_%'`).Scan(&capabilities.HNSWProfiles); err != nil {
		return domain.VectorSearchCapabilities{}, err
	}
	return capabilities, nil
}

const embeddingSummarySelect = `SELECT e.id, e.bucket, e.key, e.source_checksum, e.source_updated_at, e.plugin_id, e.kind, e.model, e.model_version, e.metric, e.dimensions, e.ordinal, e.metadata_json, e.created_at, e.updated_at`

func scanEmbeddingSummaryWithScore(row scanner) (domain.ObjectEmbedding, float64, error) {
	var embedding domain.ObjectEmbedding
	var metadataJSON, sourceUpdatedAt, createdAt, updatedAt string
	var score float64
	if err := row.Scan(&embedding.ID, &embedding.Bucket, &embedding.Key, &embedding.SourceChecksum, &sourceUpdatedAt,
		&embedding.PluginID, &embedding.Kind, &embedding.Model, &embedding.ModelVersion, &embedding.Metric,
		&embedding.Dimensions, &embedding.Ordinal, &metadataJSON, &createdAt, &updatedAt, &score); err != nil {
		return embedding, 0, err
	}
	if err := jsonUnmarshalStringMap(metadataJSON, &embedding.Metadata); err != nil {
		return embedding, 0, err
	}
	embedding.SourceUpdatedAt = parseOptionalTime(sourceUpdatedAt)
	embedding.CreatedAt = parseOptionalTime(createdAt)
	embedding.UpdatedAt = parseOptionalTime(updatedAt)
	return embedding, score, nil
}

func jsonUnmarshalStringMap(raw string, target *map[string]string) error {
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return err
	}
	if *target == nil {
		*target = map[string]string{}
	}
	return nil
}

func (profile vectorProfile) hash() string {
	digest := sha256.Sum256([]byte(strings.Join([]string{profile.Kind, profile.Model, profile.ModelVersion, profile.Metric, strconv.Itoa(profile.Dimensions)}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (profile vectorProfile) lockID() int64 {
	digest := sha256.Sum256([]byte("bucketmux-pgvector-index\x00" + profile.hash()))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func formatPGVector(values []float32) string {
	var builder strings.Builder
	builder.Grow(len(values)*12 + 2)
	builder.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	builder.WriteByte(']')
	return builder.String()
}
