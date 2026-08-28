package main

import (
	"context"
	"fmt"

	"github.com/gnurub/bucketmux/sdk/go/bucketmuxplugin"
)

func main() { bucketmuxplugin.Main(handle) }

func handle(_ context.Context, invocation bucketmuxplugin.Invocation) (bucketmuxplugin.Result, error) {
	if !allowed(invocation, bucketmuxplugin.OperationMetadataPatch) {
		return bucketmuxplugin.Result{}, fmt.Errorf("metadata.patch capability is required")
	}
	operations := []bucketmuxplugin.Operation{{
		ID: "classify-source", Type: bucketmuxplugin.OperationMetadataPatch,
		Metadata: map[string]string{"pipeline-state": "classified"}, RemoveMetadata: []string{"pipeline-pending"},
	}}
	if targetKey := invocation.Config["copy_key"]; targetKey != "" {
		if !allowed(invocation, bucketmuxplugin.OperationObjectCopy) {
			return bucketmuxplugin.Result{}, fmt.Errorf("object.copy capability is required")
		}
		operations = append(operations, bucketmuxplugin.Operation{
			ID: "copy-result", Type: bucketmuxplugin.OperationObjectCopy,
			Bucket: invocation.Config["copy_bucket"], Key: targetKey,
			Metadata: map[string]string{"copied-by": "go-bucket-operator"},
		})
	}
	return bucketmuxplugin.Result{Operations: operations}, nil
}

func allowed(invocation bucketmuxplugin.Invocation, operationType string) bool {
	for _, allowedType := range invocation.Capabilities.Operations.AllowedOperations {
		if allowedType == operationType {
			return true
		}
	}
	return false
}
