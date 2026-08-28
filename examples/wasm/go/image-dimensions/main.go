package main

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/gnurub/bucketmux/sdk/go/bucketmuxplugin"
)

func main() { bucketmuxplugin.Main(handle) }

func handle(_ context.Context, invocation bucketmuxplugin.Invocation) (bucketmuxplugin.Result, error) {
	file, err := invocation.Object.Open()
	if err != nil {
		return bucketmuxplugin.Result{}, err
	}
	defer func() { _ = file.Close() }()
	config, format, err := image.DecodeConfig(file)
	if err != nil {
		return bucketmuxplugin.Result{}, fmt.Errorf("decode image header: %w", err)
	}
	return bucketmuxplugin.Result{Metadata: map[string]string{
		"image-width": fmt.Sprint(config.Width), "image-height": fmt.Sprint(config.Height), "image-format": format,
	}}, nil
}
