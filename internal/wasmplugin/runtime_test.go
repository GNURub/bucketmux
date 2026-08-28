package wasmplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestSafeOutputPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, candidate := range []string{"", ".", "..", "../secret", "/etc/passwd"} {
		if _, err := safeOutputPath(root, candidate); err == nil {
			t.Errorf("safeOutputPath(%q) accepted an unsafe path", candidate)
		}
	}
	path, err := safeOutputPath(root, "nested/output.webp")
	if err != nil || path != filepath.Join(root, "nested", "output.webp") {
		t.Fatalf("safeOutputPath(valid) = %q, %v", path, err)
	}
}

func TestGoBuiltWASMExamples(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_WASM_EXAMPLES") == "" {
		t.Skip("set BUCKETMUX_RUN_WASM_EXAMPLES=1 after building examples")
	}
	runtime := NewRuntime(t.TempDir())
	plugin := domain.WASMPlugin{ID: "go-example", Name: "Go example", ABIVersion: domain.WASMPluginABIV1, TimeoutMillis: 20_000, MemoryLimitBytes: 256 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 3, OperationPolicy: domain.WASMPluginOperationPolicy{AllowedOperations: []string{domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationObjectCopy}, BucketPatterns: []string{"archive"}, MaxOperations: 4}}
	invocation := domain.WASMPluginInvocation{Event: domain.WASMPluginEventObjectCreated, JobID: "go-job", Object: domain.WASMPluginObject{Bucket: "images", Key: "hello.txt", Size: 11, ContentType: "text/plain"}}

	metadataModule := readExampleModule(t, "go", "build", "metadata-tagger.wasm")
	execution, err := runtime.Execute(context.Background(), metadataModule, plugin, invocation, strings.NewReader("hello wasm!"))
	if err != nil {
		t.Fatalf("Execute(Go metadata) error = %v", err)
	}
	if execution.Result.Tags["processed-by"] != "go-wasi" || execution.Result.Metadata["go-plugin-size"] != "11" {
		t.Fatalf("Go metadata result = %+v", execution.Result)
	}
	_ = execution.Close()

	invocation.Config = map[string]string{"copy_bucket": "archive", "copy_key": "processed/hello.txt"}
	operatorModule := readExampleModule(t, "go", "build", "bucket-operator.wasm")
	execution, err = runtime.Execute(context.Background(), operatorModule, plugin, invocation, strings.NewReader("hello wasm!"))
	if err != nil {
		t.Fatalf("Execute(Go bucket operator) error = %v", err)
	}
	if len(execution.Result.Operations) != 2 || execution.Result.Operations[1].Type != domain.WASMPluginOperationObjectCopy || execution.Result.Operations[1].Bucket != "archive" {
		t.Fatalf("Go bucket operation result = %+v", execution.Result)
	}
	_ = execution.Close()
	invocation.Config = nil

	embeddingModule := readExampleModule(t, "go", "build", "embedding-generator.wasm")
	execution, err = runtime.Execute(context.Background(), embeddingModule, plugin, invocation, strings.NewReader("hello wasm!"))
	if err != nil {
		t.Fatalf("Execute(Go embedding) error = %v", err)
	}
	if len(execution.Result.Embeddings) != 1 || execution.Result.Embeddings[0].Dimensions != 16 || len(execution.Result.Embeddings[0].Values) != 16 || execution.Result.Embeddings[0].Model != "byte-histogram" {
		t.Fatalf("Go embedding result = %+v", execution.Result)
	}
	_ = execution.Close()

	var imageBytes bytes.Buffer
	fixture := image.NewRGBA(image.Rect(0, 0, 7, 5))
	fixture.Set(0, 0, color.White)
	if err := png.Encode(&imageBytes, fixture); err != nil {
		t.Fatal(err)
	}
	invocation.Object.Key = "fixture.png"
	invocation.Object.ContentType = "image/png"
	invocation.Object.Size = int64(imageBytes.Len())
	imageModule := readExampleModule(t, "go", "build", "image-dimensions.wasm")
	execution, err = runtime.Execute(context.Background(), imageModule, plugin, invocation, bytes.NewReader(imageBytes.Bytes()))
	if err != nil {
		t.Fatalf("Execute(Go image) error = %v", err)
	}
	defer func() { _ = execution.Close() }()
	if execution.Result.Metadata["image-width"] != "7" || execution.Result.Metadata["image-height"] != "5" || execution.Result.Metadata["image-format"] != "png" {
		t.Fatalf("Go image result = %+v", execution.Result)
	}
}

func TestLimitedBufferCapsUntrustedOutput(t *testing.T) {
	buffer := newLimitedBuffer(4)
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if !buffer.overflowed || buffer.String() != "abcd" {
		t.Fatalf("buffer = %q overflowed=%v", buffer.String(), buffer.overflowed)
	}
}

func TestValidateEmbeddingsRejectsInvalidModelShapeAndValues(t *testing.T) {
	tests := []struct {
		name       string
		embeddings []domain.WASMPluginEmbedding
	}{
		{name: "missing provenance", embeddings: []domain.WASMPluginEmbedding{{Values: []float32{1}}}},
		{name: "shape mismatch", embeddings: []domain.WASMPluginEmbedding{{Model: "m", ModelVersion: "1", Dimensions: 2, Values: []float32{1}}}},
		{name: "metric", embeddings: []domain.WASMPluginEmbedding{{Model: "m", ModelVersion: "1", Metric: "hamming", Values: []float32{1}}}},
		{name: "not finite", embeddings: []domain.WASMPluginEmbedding{{Model: "m", ModelVersion: "1", Values: []float32{float32(math.Inf(1))}}}},
		{name: "cosine zero vector", embeddings: []domain.WASMPluginEmbedding{{Model: "m", ModelVersion: "1", Metric: "cosine", Values: []float32{0, 0}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEmbeddings(test.embeddings); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	valid := []domain.WASMPluginEmbedding{{Model: "m", ModelVersion: "1", Values: []float32{1, 0}}}
	if err := validateEmbeddings(valid); err != nil || valid[0].Kind != "generic" || valid[0].Metric != "cosine" || valid[0].Dimensions != 2 {
		t.Fatalf("normalized embedding = %+v, err = %v", valid, err)
	}
}

func TestRustAndBunBuiltWASMExamples(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_WASM_EXAMPLES") == "" {
		t.Skip("set BUCKETMUX_RUN_WASM_EXAMPLES=1 after building examples")
	}
	runtime := NewRuntime(t.TempDir())
	plugin := domain.WASMPlugin{ID: "example", Name: "Example", ABIVersion: domain.WASMPluginABIV1, TimeoutMillis: 10_000, MemoryLimitBytes: 128 << 20, MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxAttempts: 3, OperationPolicy: domain.WASMPluginOperationPolicy{AllowedOperations: []string{domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationTagsPatch}, MaxOperations: 4}}
	invocation := domain.WASMPluginInvocation{Event: domain.WASMPluginEventObjectCreated, JobID: "job", Object: domain.WASMPluginObject{Bucket: "images", Key: "hello.txt", Size: 11, ContentType: "text/plain"}}

	rustModule := readExampleModule(t, "rust", "target", "wasm32-wasip1", "release", "uppercase-transform.wasm")
	execution, err := runtime.Execute(context.Background(), rustModule, plugin, invocation, strings.NewReader("hello wasm!"))
	if err != nil {
		t.Fatalf("Execute(Rust) error = %v", err)
	}
	defer func() { _ = execution.Close() }()
	derived, err := execution.OpenDerived("uppercase.txt")
	if err != nil {
		t.Fatalf("OpenDerived() error = %v", err)
	}
	data, readErr := io.ReadAll(derived)
	_ = derived.Close()
	if readErr != nil || string(data) != "HELLO WASM!" {
		t.Fatalf("Rust derived data = %q, %v", data, readErr)
	}
	if execution.Result.Tags["transformed"] != "true" || len(execution.Result.DerivedObjects) != 1 {
		t.Fatalf("Rust result = %+v", execution.Result)
	}

	rustClassifier := readExampleModule(t, "rust", "target", "wasm32-wasip1", "release", "image-classifier.wasm")
	execution, err = runtime.Execute(context.Background(), rustClassifier, plugin, invocation, strings.NewReader("hello wasm!"))
	if err != nil {
		t.Fatalf("Execute(Rust classifier) error = %v", err)
	}
	if len(execution.Result.Operations) != 1 || execution.Result.Operations[0].Type != domain.WASMPluginOperationMetadataPatch {
		t.Fatalf("Rust classifier operations = %+v", execution.Result.Operations)
	}
	_ = execution.Close()

	bunModule := readExampleModule(t, "bun-assemblyscript", "build", "image-classifier.wasm")
	invocation.Object.Size = 3
	execution, err = runtime.Execute(context.Background(), bunModule, plugin, invocation, strings.NewReader("img"))
	if err != nil {
		t.Fatalf("Execute(Bun/AssemblyScript) error = %v", err)
	}
	defer func() { _ = execution.Close() }()
	encoded, _ := json.Marshal(execution.Result)
	if execution.Result.Tags["media-category"] != "image" {
		t.Fatalf("Bun/AssemblyScript result = %s", encoded)
	}
	if len(execution.Result.Operations) != 1 || execution.Result.Operations[0].Type != domain.WASMPluginOperationTagsPatch {
		t.Fatalf("Bun/AssemblyScript operations = %+v", execution.Result.Operations)
	}

	timeoutModule := readExampleModule(t, "bun-assemblyscript", "build", "timeout-fixture.wasm")
	plugin.TimeoutMillis = 25
	started := time.Now()
	if _, err := runtime.Execute(context.Background(), timeoutModule, plugin, invocation, strings.NewReader("img")); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout fixture error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout fixture took %v", elapsed)
	}
}

func readExampleModule(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "examples", "wasm"}, parts...)...)
	module, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example module %s: %v", path, err)
	}
	return module
}
