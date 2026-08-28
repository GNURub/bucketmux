package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestObjectEmbeddingsReplaceNativeSearchAndCascadeTurso(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "embeddings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "local", Name: "local", Kind: domain.ProviderKindLocal, Bucket: "data", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	put := func(key, checksum string) domain.ObjectRecord {
		object := domain.ObjectRecord{Bucket: "photos", Key: key, ProviderAccountID: "local", RemoteBucket: "data", RemoteKey: key, Size: 3, ChecksumSHA256: checksum}
		if err := s.PutObject(ctx, object); err != nil {
			t.Fatal(err)
		}
		object, err = s.GetObject(ctx, object.Bucket, object.Key)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	alice := put("alice.jpg", "alice-v1")
	bob := put("bob.jpg", "bob-v1")
	face := func(values ...float32) []domain.WASMPluginEmbedding {
		return []domain.WASMPluginEmbedding{{Kind: "face", Model: "arcface", ModelVersion: "2026-01", Metric: "cosine", Dimensions: len(values), Values: values, Metadata: map[string]string{"box": "1,2,3,4"}}}
	}
	if err := s.ReplaceObjectEmbeddings(ctx, alice, "faces", face(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceObjectEmbeddings(ctx, bob, "faces", face(0.8, 0.2, 0)); err != nil {
		t.Fatal(err)
	}

	summaries, err := s.ListObjectEmbeddings(ctx, "photos", "alice.jpg", false)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries = %#v, err = %v", summaries, err)
	}
	if summaries[0].Values != nil || summaries[0].Dimensions != 3 || summaries[0].Metadata["box"] == "" {
		t.Fatalf("unexpected summary: %#v", summaries[0])
	}
	full, err := s.ListObjectEmbeddings(ctx, "photos", "alice.jpg", true)
	if err != nil || len(full) != 1 || len(full[0].Values) != 3 || full[0].Values[0] != 1 {
		t.Fatalf("full embeddings = %#v, err = %v", full, err)
	}

	results, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Bucket: "photos", Kind: "face", Model: "arcface", ModelVersion: "2026-01", Values: []float32{1, 0, 0}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Embedding.Key != "alice.jpg" || results[0].Embedding.Values != nil || math.Abs(results[0].Score-1) > 1e-6 || results[1].Embedding.Key != "bob.jpg" {
		t.Fatalf("search results = %#v", results)
	}

	if err := s.ReplaceObjectEmbeddings(ctx, alice, "faces", face(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	replaced, err := s.ListObjectEmbeddings(ctx, "photos", "alice.jpg", true)
	if err != nil || len(replaced) != 1 || replaced[0].Values[1] != 1 {
		t.Fatalf("replaced = %#v, err = %v", replaced, err)
	}
	if err := s.DeleteObject(ctx, "photos", "alice.jpg"); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.ListObjectEmbeddings(ctx, "photos", "alice.jpg", true)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("cascade results = %#v, err = %v", deleted, err)
	}
}

func TestObjectEmbeddingValidation(t *testing.T) {
	if _, err := encodeFloat32Vector([]float32{float32(math.NaN())}); err == nil {
		t.Fatal("expected non-finite value error")
	}
	query := domain.EmbeddingSearchQuery{Model: "arcface", Values: []float32{1}, Metric: "hamming"}
	if err := normalizeEmbeddingSearchQuery(&query); err == nil {
		t.Fatal("expected unsupported metric error")
	}
}

func TestTursoNativeVectorMetricsCapabilitiesAndStartupBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turso-vectors.db")
	s, err := OpenTurso(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "local", Name: "local", Kind: domain.ProviderKindLocal, Bucket: "data", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	object := domain.ObjectRecord{Bucket: "vectors", Key: "fixture", ProviderAccountID: "local", RemoteBucket: "data", RemoteKey: "fixture", Size: 1, ChecksumSHA256: "fixture-v1"}
	if err := s.PutObject(ctx, object); err != nil {
		t.Fatal(err)
	}
	object, err = s.GetObject(ctx, object.Bucket, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	embeddings := []domain.WASMPluginEmbedding{
		{Kind: "fixture", Model: "dot-model", ModelVersion: "1", Metric: "dot", Dimensions: 3, Values: []float32{2, 0, 0}},
		{Kind: "fixture", Model: "l2-model", ModelVersion: "1", Metric: "l2", Dimensions: 3, Values: []float32{1, 1, 0}},
	}
	if err := s.ReplaceObjectEmbeddings(ctx, object, "metrics", embeddings); err != nil {
		t.Fatal(err)
	}

	dot, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Model: "dot-model", ModelVersion: "1", Kind: "fixture", Metric: "dot", Values: []float32{1, 0, 0}, Limit: 1})
	if err != nil || len(dot) != 1 || math.Abs(dot[0].Score-2) > 1e-6 {
		t.Fatalf("native dot search = %#v, err = %v", dot, err)
	}
	minimum := 2.1
	filtered, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Model: "dot-model", ModelVersion: "1", Kind: "fixture", Metric: "dot", Values: []float32{1, 0, 0}, Limit: 1, MinScore: &minimum})
	if err != nil || len(filtered) != 0 {
		t.Fatalf("native minimum-score filter = %#v, err = %v", filtered, err)
	}
	l2, err := s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Model: "l2-model", ModelVersion: "1", Kind: "fixture", Metric: "l2", Values: []float32{1, 0, 0}, Limit: 1})
	if err != nil || len(l2) != 1 || math.Abs(l2[0].Score-(-1)) > 1e-6 {
		t.Fatalf("native l2 search = %#v, err = %v", l2, err)
	}
	capabilities, err := s.VectorSearchCapabilities(ctx)
	if err != nil || capabilities.Backend != "turso-native-exact" || capabilities.Engine != "turso" || capabilities.ANN {
		t.Fatalf("Turso capabilities = %#v, err = %v", capabilities, err)
	}
	var extracted string
	if err := s.queryRow(ctx, `SELECT vector_extract(embedding) FROM object_embedding_turso_vectors WHERE embedding_id = ?`, dot[0].Embedding.ID).Scan(&extracted); err != nil || extracted == "" {
		t.Fatalf("extract native vector = %q, err = %v", extracted, err)
	}
	if _, err := s.exec(ctx, `DELETE FROM object_embedding_turso_vectors WHERE embedding_id = ?`, dot[0].Embedding.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenTurso(path)
	if err != nil {
		t.Fatalf("reopen and backfill Turso database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dot, err = s.SearchObjectEmbeddings(ctx, domain.EmbeddingSearchQuery{Model: "dot-model", ModelVersion: "1", Kind: "fixture", Metric: "dot", Values: []float32{1, 0, 0}, Limit: 1})
	if err != nil || len(dot) != 1 || math.Abs(dot[0].Score-2) > 1e-6 {
		t.Fatalf("backfilled native dot search = %#v, err = %v", dot, err)
	}
}
