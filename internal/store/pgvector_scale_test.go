package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestPostgresPGVectorHNSWScaleIntegration(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_PGVECTOR_SCALE") != "1" {
		t.Skip("set BUCKETMUX_RUN_PGVECTOR_SCALE=1 to run")
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("POSTGRES_DSN is required")
	}
	count := 10_000
	if raw := os.Getenv("BUCKETMUX_PGVECTOR_SCALE_COUNT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 100 {
			t.Fatalf("invalid BUCKETMUX_PGVECTOR_SCALE_COUNT %q", raw)
		}
		count = parsed
	}
	s, err := openPostgres(config.PostgresStoreConfig{DSN: dsn, MaxOpenConns: 10, MaxIdleConns: 5}, config.VectorSearchConfig{
		Backend: "pgvector", HNSWM: 16, EFConstruction: 64, EFSearch: 100, MaxScanTuples: 20_000, MaxProfiles: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	providerID := "pgvector-scale-provider-" + suffix
	bucket := "pgvector-scale-" + suffix
	key := "faces.jpg"
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: providerID, Name: providerID, Kind: domain.ProviderKindLocal, Bucket: bucket, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteProvider(context.Background(), providerID) })
	if err := s.PutObject(ctx, domain.ObjectRecord{Bucket: bucket, Key: key, ProviderAccountID: providerID, RemoteBucket: bucket, RemoteKey: key, Size: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteObject(context.Background(), bucket, key) })
	object, err := s.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	profile := vectorProfile{Kind: "face", Model: "arcface-scale", ModelVersion: suffix, Metric: "cosine", Dimensions: 512}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseStatement, err := tx.PrepareContext(ctx, s.rebind(`
INSERT INTO object_embeddings (id, bucket, key, source_checksum, source_updated_at, plugin_id, kind, model, model_version, metric, dimensions, ordinal, values_blob, metadata_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?)`))
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	vectorStatement, err := tx.PrepareContext(ctx, `
INSERT INTO object_embedding_vectors (embedding_id, profile_hash, bucket, key, dimensions, metric, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7::vector)`)
	if err != nil {
		_ = baseStatement.Close()
		_ = tx.Rollback()
		t.Fatal(err)
	}
	startedInsert := time.Now()
	queryVector := make([]float32, profile.Dimensions)
	queryVector[0] = 1
	for i := range count {
		values := make([]float32, profile.Dimensions)
		values[0] = 1
		values[1] = float32(i) / float32(count)
		blob, err := encodeFloat32Vector(values)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("emb-scale-%s-%06d", suffix, i)
		if _, err := baseStatement.ExecContext(ctx, id, bucket, key, object.ChecksumSHA256, optionalTimeString(object.UpdatedAt), "scale-plugin", profile.Kind, profile.Model, profile.ModelVersion, profile.Metric, profile.Dimensions, i, blob, now, now); err != nil {
			_ = vectorStatement.Close()
			_ = baseStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert base vector %d: %v", i, err)
		}
		if _, err := vectorStatement.ExecContext(ctx, id, profile.hash(), bucket, key, profile.Dimensions, profile.Metric, formatPGVector(values)); err != nil {
			_ = vectorStatement.Close()
			_ = baseStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert pgvector %d: %v", i, err)
		}
	}
	_ = vectorStatement.Close()
	_ = baseStatement.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("inserted %d 512-dimensional vectors in %s", count, time.Since(startedInsert))
	startedIndex := time.Now()
	if err := s.ensurePGVectorIndex(ctx, profile); err != nil {
		t.Fatal(err)
	}
	t.Logf("built HNSW profile in %s", time.Since(startedIndex))
	if _, err := s.exec(ctx, `ANALYZE object_embedding_vectors`); err != nil {
		t.Fatal(err)
	}
	startedSearch := time.Now()
	results, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Bucket: bucket, Kind: profile.Kind, Model: profile.Model, ModelVersion: profile.ModelVersion, Metric: profile.Metric, Values: queryVector, Limit: 10})
	if err != nil || len(results) != 10 || results[0].Score < 0.999999 {
		t.Fatalf("HNSW search results=%d first=%+v err=%v", len(results), results[0], err)
	}
	t.Logf("HNSW top-10 search over %d vectors took %s", count, time.Since(startedSearch))

	explainTx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = explainTx.Rollback() }()
	if _, err := explainTx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	explainRows, err := explainTx.QueryContext(ctx, fmt.Sprintf(`EXPLAIN SELECT embedding_id FROM object_embedding_vectors WHERE profile_hash = '%s' ORDER BY embedding::vector(%d) <=> $1::vector LIMIT 10`, profile.hash(), profile.Dimensions), formatPGVector(queryVector))
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for explainRows.Next() {
		var line string
		if err := explainRows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	_ = explainRows.Close()
	if !strings.Contains(plan.String(), "idx_embedding_hnsw_"+profile.hash()[:20]) {
		t.Fatalf("query plan does not use HNSW index:\n%s", plan.String())
	}
}
