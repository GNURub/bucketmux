package main

import (
	"context"
	"io"
	"math"

	"github.com/gnurub/bucketmux/sdk/go/bucketmuxplugin"
)

const dimensions = 16

func main() { bucketmuxplugin.Main(handle) }

// This deterministic histogram is an integration fixture, not a recognition
// model. A production face plugin should use an approved detector plus an
// ArcFace-compatible embedding model and preserve that model's exact version.
func handle(_ context.Context, invocation bucketmuxplugin.Invocation) (bucketmuxplugin.Result, error) {
	file, err := invocation.Object.Open()
	if err != nil {
		return bucketmuxplugin.Result{}, err
	}
	defer func() { _ = file.Close() }()
	values := make([]float32, dimensions)
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := file.Read(buffer)
		for _, value := range buffer[:read] {
			values[int(value)*dimensions/256]++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return bucketmuxplugin.Result{}, readErr
		}
	}
	var norm float64
	for _, value := range values {
		norm += float64(value * value)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range values {
			values[i] = float32(float64(values[i]) / norm)
		}
	} else {
		values[0] = 1
	}
	return bucketmuxplugin.Result{Embeddings: []bucketmuxplugin.Embedding{{
		Kind: "demo-content", Model: "byte-histogram", ModelVersion: "1.0.0", Metric: "cosine",
		Dimensions: dimensions, Values: values, Metadata: map[string]string{"fixture": "true"},
	}}}, nil
}
