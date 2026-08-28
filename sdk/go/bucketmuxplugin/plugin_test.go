package bucketmuxplugin

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunDefaultsABIVersionAndEncodesEmbedding(t *testing.T) {
	input := `{"abi_version":"bucketmux.wasm.v1","event":"object.created","object":{"bucket":"photos","key":"a.jpg","input_path":"/input/object"}}`
	var output bytes.Buffer
	err := Run(context.Background(), strings.NewReader(input), &output, func(_ context.Context, invocation Invocation) (Result, error) {
		if invocation.Object.Key != "a.jpg" {
			t.Fatalf("key = %q", invocation.Object.Key)
		}
		return Result{Embeddings: []Embedding{{Kind: "face", Model: "demo", ModelVersion: "1", Metric: "cosine", Dimensions: 2, Values: []float32{0.25, 0.75}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"abi_version":"bucketmux.wasm.v1"`) || !strings.Contains(output.String(), `"values":[0.25,0.75]`) {
		t.Fatalf("unexpected output: %s", output.String())
	}
}

func TestRunRejectsUnknownABI(t *testing.T) {
	err := Run(context.Background(), strings.NewReader(`{"abi_version":"future"}`), &bytes.Buffer{}, func(context.Context, Invocation) (Result, error) { return Result{}, nil })
	if err == nil || !strings.Contains(err.Error(), "unsupported BucketMux ABI") {
		t.Fatalf("error = %v", err)
	}
}
