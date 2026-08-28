package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestTursoNativeVectorScaleIntegration(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_TURSO_SCALE") != "1" {
		t.Skip("set BUCKETMUX_RUN_TURSO_SCALE=1 to run")
	}
	count := 10_000
	if raw := os.Getenv("BUCKETMUX_TURSO_SCALE_COUNT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 100 || parsed > maximumEmbeddingCandidates {
			t.Fatalf("invalid BUCKETMUX_TURSO_SCALE_COUNT %q", raw)
		}
		count = parsed
	}
	s, err := OpenTurso(t.TempDir() + "/scale.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "scale", Name: "scale", Kind: domain.ProviderKindLocal, Bucket: "scale", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObject(ctx, domain.ObjectRecord{Bucket: "scale", Key: "faces.jpg", ProviderAccountID: "scale", RemoteBucket: "scale", RemoteKey: "faces.jpg", Size: 1}); err != nil {
		t.Fatal(err)
	}
	object, err := s.GetObject(ctx, "scale", "faces.jpg")
	if err != nil {
		t.Fatal(err)
	}
	profile := vectorProfile{Kind: "face", Model: "arcface-turso-scale", ModelVersion: "1", Metric: "cosine", Dimensions: 512}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseStatement, err := tx.PrepareContext(ctx, `
INSERT INTO object_embeddings (id, bucket, key, source_checksum, source_updated_at, plugin_id, kind, model, model_version, metric, dimensions, ordinal, values_blob, metadata_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	vectorStatement, err := tx.PrepareContext(ctx, `
INSERT INTO object_embedding_turso_vectors (embedding_id, profile_hash, bucket, key, dimensions, metric, embedding)
VALUES (?, ?, ?, ?, ?, ?, vector32(?))`)
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
		id := fmt.Sprintf("emb-turso-scale-%06d", i)
		if _, err := baseStatement.ExecContext(ctx, id, object.Bucket, object.Key, object.ChecksumSHA256, optionalTimeString(object.UpdatedAt), "scale-plugin", profile.Kind, profile.Model, profile.ModelVersion, profile.Metric, profile.Dimensions, i, blob, now, now); err != nil {
			_ = vectorStatement.Close()
			_ = baseStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert portable vector %d: %v", i, err)
		}
		if _, err := vectorStatement.ExecContext(ctx, id, profile.hash(), object.Bucket, object.Key, profile.Dimensions, profile.Metric, formatPGVector(values)); err != nil {
			_ = vectorStatement.Close()
			_ = baseStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert Turso vector %d: %v", i, err)
		}
	}
	_ = vectorStatement.Close()
	_ = baseStatement.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("inserted %d native Turso vectors with 512 dimensions in %s", count, time.Since(startedInsert))

	startedSearch := time.Now()
	results, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{
		Bucket: object.Bucket, Kind: profile.Kind, Model: profile.Model, ModelVersion: profile.ModelVersion,
		Metric: profile.Metric, Values: queryVector, Limit: 10, MaxCandidates: count,
	})
	if err != nil || len(results) != 10 || results[0].Score < 0.9999 {
		t.Fatalf("Turso search results=%d first=%+v err=%v", len(results), results[0], err)
	}
	t.Logf("native exact top-10 search over %d vectors took %s", count, time.Since(startedSearch))
}
