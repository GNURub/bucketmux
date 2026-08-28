package app

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/wasmplugin"
)

func TestWASMPluginSelectors(t *testing.T) {
	plugin := domain.WASMPlugin{Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "media-*", KeyPrefix: "uploads/", KeySuffix: ".jpg", ContentTypes: []string{"image/*"}}
	matching := domain.ObjectRecord{Bucket: "media-eu", Key: "uploads/portrait.jpg", ContentType: "image/jpeg; charset=binary"}
	if !wasmPluginMatches(plugin, domain.WASMPluginEventObjectCreated, matching) {
		t.Fatal("matching object was rejected")
	}
	for name, object := range map[string]domain.ObjectRecord{
		"bucket":       {Bucket: "documents", Key: matching.Key, ContentType: matching.ContentType},
		"prefix":       {Bucket: matching.Bucket, Key: "portrait.jpg", ContentType: matching.ContentType},
		"suffix":       {Bucket: matching.Bucket, Key: "uploads/portrait.png", ContentType: matching.ContentType},
		"content type": {Bucket: matching.Bucket, Key: matching.Key, ContentType: "video/mp4"},
	} {
		t.Run(name, func(t *testing.T) {
			if wasmPluginMatches(plugin, domain.WASMPluginEventObjectCreated, object) {
				t.Fatalf("non-matching object was accepted: %+v", object)
			}
		})
	}
}

func TestWASMPluginRetriesAndRaisesAlertAfterExhaustion(t *testing.T) {
	dataDir := t.TempDir()
	svc := newWASMTestService(t, dataDir, filepath.Join(dataDir, "retry.db"))
	if svc.cancelWorkers != nil {
		svc.cancelWorkers()
		svc.workerWG.Wait()
	}
	svc.WASMRuntime = failingWASMExecutor{err: errors.New("model temporarily unavailable")}
	plugin := domain.WASMPlugin{ID: "failing", Name: "Failing", ABIVersion: domain.WASMPluginABIV1, ModuleBase64: base64.StdEncoding.EncodeToString([]byte("module")), Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "*", Enabled: true, TimeoutMillis: 1000, MemoryLimitBytes: 1 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 2}
	if err := svc.Store.UpsertWASMPlugin(context.Background(), plugin); err != nil {
		t.Fatalf("UpsertWASMPlugin() error = %v", err)
	}
	if _, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "retry.txt", Size: 4, ContentType: "text/plain"}, strings.NewReader("data")); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	job, claimed, err := svc.Store.ClaimNextWASMPluginJob(context.Background(), time.Now().UTC().Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("ClaimNextWASMPluginJob(first) = %+v, %v, %v", job, claimed, err)
	}
	if err := svc.runWASMPluginJob(context.Background(), job); err == nil {
		t.Fatal("first failing execution returned nil")
	}
	job, _ = svc.Store.GetWASMPluginJob(context.Background(), job.ID)
	if job.Status != domain.WASMPluginStatusPending || job.Attempts != 1 {
		t.Fatalf("first retry job = %+v", job)
	}
	job.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	if err := svc.Store.UpdateWASMPluginJob(context.Background(), job); err != nil {
		t.Fatalf("UpdateWASMPluginJob() error = %v", err)
	}
	job, claimed, err = svc.Store.ClaimNextWASMPluginJob(context.Background(), time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("ClaimNextWASMPluginJob(second) = %+v, %v, %v", job, claimed, err)
	}
	if err := svc.runWASMPluginJob(context.Background(), job); err == nil {
		t.Fatal("second failing execution returned nil")
	}
	job, _ = svc.Store.GetWASMPluginJob(context.Background(), job.ID)
	if job.Status != domain.WASMPluginStatusFailed || job.Attempts != 2 {
		t.Fatalf("exhausted job = %+v", job)
	}
	alerts, err := svc.Store.ListAlerts(context.Background(), domain.AlertStatusOpen, 10)
	if err != nil || len(alerts) != 1 || alerts[0].Type != domain.AlertTypeWASMPluginFailed {
		t.Fatalf("alerts = %+v, %v", alerts, err)
	}
}

type failingWASMExecutor struct{ err error }

func (failingWASMExecutor) Validate(context.Context, []byte, domain.WASMPlugin) error { return nil }
func (executor failingWASMExecutor) Execute(context.Context, []byte, domain.WASMPlugin, domain.WASMPluginInvocation, io.Reader) (*wasmplugin.Execution, error) {
	return nil, executor.err
}

func TestWASMPipelineSingleAndMultipleInstances(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_WASM_EXAMPLES") == "" {
		t.Skip("set BUCKETMUX_RUN_WASM_EXAMPLES=1 after building examples")
	}
	t.Run("single", func(t *testing.T) {
		dataDir := t.TempDir()
		svc := newWASMTestService(t, dataDir, filepath.Join(dataDir, "single.db"))
		installRustUppercasePlugin(t, svc)
		object, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "hello.txt", Size: 11, ContentType: "text/plain"}, strings.NewReader("hello wasm!"))
		if err != nil {
			t.Fatalf("PutObject() error = %v", err)
		}
		job := waitForWASMJob(t, svc, domain.WASMPluginStatusSucceeded)
		if job.Attempts != 1 || job.SourceChecksum != object.ChecksumSHA256 {
			t.Fatalf("job = %+v", job)
		}
		assertWASMResults(t, svc)
		if _, err := svc.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "hello.txt", Size: 11, ContentType: "text/plain"}, strings.NewReader("hello wasm!")); err != nil {
			t.Fatalf("identical overwrite PutObject() error = %v", err)
		}
		jobs := waitForWASMJobCount(t, svc, 2)
		if jobs[0].DedupeKey == jobs[1].DedupeKey || jobs[0].Status != domain.WASMPluginStatusSucceeded || jobs[1].Status != domain.WASMPluginStatusSucceeded {
			t.Fatalf("identical overwrite jobs = %+v", jobs)
		}
	})

	t.Run("multiple", func(t *testing.T) {
		dataDir := t.TempDir()
		dbPath := filepath.Join(dataDir, "multiple.db")
		first := newWASMTestService(t, dataDir, dbPath)
		second := newWASMTestService(t, dataDir, dbPath)
		installRustUppercasePlugin(t, first)
		if _, err := first.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "hello.txt", Size: 11, ContentType: "text/plain"}, strings.NewReader("hello wasm!")); err != nil {
			t.Fatalf("PutObject() error = %v", err)
		}
		job := waitForWASMJob(t, second, domain.WASMPluginStatusSucceeded)
		if job.Attempts != 1 {
			t.Fatalf("job was claimed more than once: %+v", job)
		}
		jobs, err := first.Store.ListWASMPluginJobs(context.Background(), 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %+v, %v", jobs, err)
		}
		assertWASMResults(t, second)
	})
}

func TestGoEmbeddingPipelineSingleAndMultipleInstances(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_WASM_EXAMPLES") == "" {
		t.Skip("set BUCKETMUX_RUN_WASM_EXAMPLES=1 after building examples")
	}
	for _, instanceMode := range []string{"single", "multiple"} {
		t.Run(instanceMode, func(t *testing.T) {
			dataDir := t.TempDir()
			dbPath := filepath.Join(dataDir, instanceMode+".db")
			writer := newWASMTestService(t, dataDir, dbPath)
			reader := writer
			if instanceMode == "multiple" {
				reader = newWASMTestService(t, dataDir, dbPath)
			}
			installGoEmbeddingPlugin(t, writer)
			if _, err := writer.PutObject(context.Background(), domain.PutObjectInput{Bucket: "images", Key: "faces/alice.bin", Size: 11, ContentType: "application/octet-stream"}, strings.NewReader("hello wasm!")); err != nil {
				t.Fatalf("PutObject() error = %v", err)
			}
			job := waitForWASMJob(t, reader, domain.WASMPluginStatusSucceeded)
			if job.Attempts != 1 || strings.Contains(job.ResultJSON, `"values"`) {
				t.Fatalf("job must succeed once without exposing vectors: %+v", job)
			}
			embeddings, err := reader.ListObjectEmbeddings(context.Background(), "images", "faces/alice.bin")
			if err != nil || len(embeddings) != 1 || embeddings[0].Values != nil || embeddings[0].Dimensions != 16 || embeddings[0].Model != "byte-histogram" {
				t.Fatalf("embeddings = %+v, err = %v", embeddings, err)
			}
			full, err := reader.Store.ListObjectEmbeddings(context.Background(), "images", "faces/alice.bin", true)
			if err != nil || len(full) != 1 {
				t.Fatalf("full embeddings = %+v, err = %v", full, err)
			}
			results, err := writer.SearchObjectEmbeddings(context.Background(), domain.EmbeddingSearchQuery{Bucket: "images", Kind: "demo-content", Model: "byte-histogram", ModelVersion: "1.0.0", Values: full[0].Values, Limit: 1})
			if err != nil || len(results) != 1 || results[0].Embedding.Key != "faces/alice.bin" || results[0].Score < 0.999999 {
				t.Fatalf("search results = %+v, err = %v", results, err)
			}
		})
	}
}

func TestPutObjectInvalidatesEmbeddingsFromPreviousGeneration(t *testing.T) {
	dataDir := t.TempDir()
	svc := newWASMTestService(t, dataDir, filepath.Join(dataDir, "invalidate.db"))
	if svc.cancelWorkers != nil {
		svc.cancelWorkers()
		svc.workerWG.Wait()
	}
	ctx := context.Background()
	if _, err := svc.PutObject(ctx, domain.PutObjectInput{Bucket: "images", Key: "face.bin", Size: 3}, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	object, err := svc.Store.GetObject(ctx, "images", "face.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.ReplaceObjectEmbeddings(ctx, object, "faces", []domain.WASMPluginEmbedding{{Kind: "face", Model: "arcface", ModelVersion: "1", Metric: "cosine", Dimensions: 2, Values: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutObject(ctx, domain.PutObjectInput{Bucket: "images", Key: "face.bin", Size: 3}, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	embeddings, err := svc.Store.ListObjectEmbeddings(ctx, "images", "face.bin", true)
	if err != nil || len(embeddings) != 0 {
		t.Fatalf("stale embeddings = %+v, err = %v", embeddings, err)
	}
}

func waitForWASMJobCount(t *testing.T, svc *Service, count int) []domain.WASMPluginJob {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := svc.Store.ListWASMPluginJobs(context.Background(), 10)
		if err == nil && len(jobs) == count {
			allFinished := true
			for _, job := range jobs {
				allFinished = allFinished && job.Status == domain.WASMPluginStatusSucceeded
			}
			if allFinished {
				return jobs
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, _ := svc.Store.ListWASMPluginJobs(context.Background(), 10)
	t.Fatalf("WASM jobs did not reach count/status: %+v", jobs)
	return nil
}

func newWASMTestService(t *testing.T, dataDir, dbPath string) *Service {
	t.Helper()
	svc, err := NewService(context.Background(), config.Config{
		Server:      config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: dbPath, MasterKey: "test-master-key"},
		S3:          config.S3Config{AccessKey: "ak", SecretKey: "sk"},
		WASMPlugins: config.WASMPluginConfig{Enabled: true},
		Buckets:     []config.BucketConfig{{Name: "images"}},
		Providers:   []config.ProviderConfig{{ID: "local", Name: "Local", Kind: string(domain.ProviderKindLocal), Bucket: "images", CapacityBytes: 1 << 30, Priority: 1, Enabled: new(true), Settings: map[string]string{"path": filepath.Join(dataDir, "objects")}}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return svc
}

func installRustUppercasePlugin(t *testing.T, svc *Service) {
	t.Helper()
	modulePath := filepath.Join("..", "..", "examples", "wasm", "rust", "target", "wasm32-wasip1", "release", "uppercase-transform.wasm")
	module, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read Rust plugin: %v", err)
	}
	err = svc.UpsertWASMPlugin(context.Background(), domain.WASMPlugin{ID: "uppercase", Name: "Uppercase", ModuleBase64: base64.StdEncoding.EncodeToString(module), Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "images", KeySuffix: ".txt", ContentTypes: []string{"text/*"}, Enabled: true})
	if err != nil {
		t.Fatalf("UpsertWASMPlugin() error = %v", err)
	}
}

func installGoEmbeddingPlugin(t *testing.T, svc *Service) {
	t.Helper()
	modulePath := filepath.Join("..", "..", "examples", "wasm", "go", "build", "embedding-generator.wasm")
	module, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read Go plugin: %v", err)
	}
	err = svc.UpsertWASMPlugin(context.Background(), domain.WASMPlugin{
		ID: "go-embedding", Name: "Go embedding", ModuleBase64: base64.StdEncoding.EncodeToString(module),
		Events: []string{domain.WASMPluginEventObjectCreated}, BucketPattern: "images", KeyPrefix: "faces/", Enabled: true,
		TimeoutMillis: 20_000, MemoryLimitBytes: 256 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("UpsertWASMPlugin(Go) error = %v", err)
	}
}

func waitForWASMJob(t *testing.T, svc *Service, status string) domain.WASMPluginJob {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := svc.Store.ListWASMPluginJobs(context.Background(), 10)
		if err == nil && len(jobs) == 1 && jobs[0].Status == status {
			return jobs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, _ := svc.Store.ListWASMPluginJobs(context.Background(), 10)
	t.Fatalf("WASM job did not reach %s: %+v", status, jobs)
	return domain.WASMPluginJob{}
}

func assertWASMResults(t *testing.T, svc *Service) {
	t.Helper()
	body, _, err := svc.GetObject(context.Background(), "images", "hello.txt.uppercase.txt")
	if err != nil {
		t.Fatalf("GetObject(derived) error = %v", err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || string(data) != "HELLO WASM!" {
		t.Fatalf("derived = %q, %v", data, readErr)
	}
	source, err := svc.Store.GetObject(context.Background(), "images", "hello.txt")
	if err != nil {
		t.Fatalf("GetObject(source) error = %v", err)
	}
	_ = svc.Store.HydrateObjectAttributes(context.Background(), &source)
	if source.Metadata["processed-by"] != "rust-uppercase" || source.Tags["transformed"] != "true" {
		t.Fatalf("source attributes = metadata=%v tags=%v", source.Metadata, source.Tags)
	}
}
