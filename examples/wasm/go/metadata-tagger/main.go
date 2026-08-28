package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/gnurub/bucketmux/sdk/go/bucketmuxplugin"
)

func main() { bucketmuxplugin.Main(handle) }

func handle(_ context.Context, invocation bucketmuxplugin.Invocation) (bucketmuxplugin.Result, error) {
	file, err := invocation.Object.Open()
	if err != nil {
		return bucketmuxplugin.Result{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return bucketmuxplugin.Result{}, err
	}
	return bucketmuxplugin.Result{
		Metadata: map[string]string{"go-plugin-sha256": fmt.Sprintf("%x", hash.Sum(nil)), "go-plugin-size": fmt.Sprint(size)},
		Tags:     map[string]string{"processed-by": "go-wasi"},
	}, nil
}
