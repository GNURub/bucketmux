// Package bucketmuxplugin implements the stable BucketMux WASI command ABI for
// plugins compiled with Go or TinyGo. A plugin reads one invocation from stdin,
// accesses the mounted object through Object.InputPath and writes one result to
// stdout.
package bucketmuxplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const ABIVersion = "bucketmux.wasm.v1"

type Handler func(context.Context, Invocation) (Result, error)

type Invocation struct {
	ABIVersion   string                 `json:"abi_version"`
	Event        string                 `json:"event"`
	JobID        string                 `json:"job_id"`
	Object       Object                 `json:"object"`
	Workspace    Workspace              `json:"workspace"`
	Config       map[string]string      `json:"config,omitempty"`
	Capabilities Capabilities           `json:"capabilities,omitempty"`
	Context      map[string]interface{} `json:"context,omitempty"`
}

type Object struct {
	Bucket         string            `json:"bucket"`
	Key            string            `json:"key"`
	Size           int64             `json:"size"`
	ContentType    string            `json:"content_type,omitempty"`
	ETag           string            `json:"etag,omitempty"`
	ChecksumSHA256 string            `json:"checksum_sha256,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	InputPath      string            `json:"input_path"`
}

func (o Object) Open() (*os.File, error) { return os.Open(o.InputPath) }

type Workspace struct {
	OutputDir string `json:"output_dir"`
}

type Result struct {
	ABIVersion     string            `json:"abi_version"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	Embeddings     []Embedding       `json:"embeddings,omitempty"`
	DerivedObjects []DerivedObject   `json:"derived_objects,omitempty"`
	Operations     []Operation       `json:"operations,omitempty"`
}

const (
	OperationMetadataPatch = "metadata.patch"
	OperationTagsPatch     = "tags.patch"
	OperationObjectCopy    = "object.copy"
	OperationObjectDelete  = "object.delete"
)

type OperationPolicy struct {
	AllowedOperations []string `json:"allowed_operations,omitempty"`
	BucketPatterns    []string `json:"bucket_patterns,omitempty"`
	KeyPrefixes       []string `json:"key_prefixes,omitempty"`
	MaxOperations     int      `json:"max_operations,omitempty"`
}

type Capabilities struct {
	Operations OperationPolicy `json:"operations,omitempty"`
}

type Operation struct {
	ID             string            `json:"id,omitempty"`
	Type           string            `json:"type"`
	Bucket         string            `json:"bucket,omitempty"`
	Key            string            `json:"key,omitempty"`
	SourceBucket   string            `json:"source_bucket,omitempty"`
	SourceKey      string            `json:"source_key,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	RemoveMetadata []string          `json:"remove_metadata,omitempty"`
	RemoveTags     []string          `json:"remove_tags,omitempty"`
}

type Embedding struct {
	Kind         string            `json:"kind"`
	Model        string            `json:"model"`
	ModelVersion string            `json:"model_version"`
	Metric       string            `json:"metric"`
	Dimensions   int               `json:"dimensions"`
	Values       []float32         `json:"values"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type DerivedObject struct {
	Path        string            `json:"path"`
	Key         string            `json:"key,omitempty"`
	KeySuffix   string            `json:"key_suffix,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("bucketmux plugin handler is nil")
	}
	var invocation Invocation
	if err := json.NewDecoder(stdin).Decode(&invocation); err != nil {
		return fmt.Errorf("decode BucketMux invocation: %w", err)
	}
	if invocation.ABIVersion != ABIVersion {
		return fmt.Errorf("unsupported BucketMux ABI %q", invocation.ABIVersion)
	}
	result, err := handler(ctx, invocation)
	if err != nil {
		return err
	}
	if result.ABIVersion == "" {
		result.ABIVersion = ABIVersion
	}
	if result.ABIVersion != ABIVersion {
		return fmt.Errorf("handler returned unsupported ABI %q", result.ABIVersion)
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("encode BucketMux result: %w", err)
	}
	return nil
}

func Main(handler Handler) {
	if err := Run(context.Background(), os.Stdin, os.Stdout, handler); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
