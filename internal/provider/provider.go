package provider

import (
	"context"
	"io"
	"os"

	"github.com/gnurub/bucketmux/internal/domain"
)

type PreparedUpload struct {
	File           *os.File
	Size           int64
	ChecksumSHA256 string
}

type Adapter interface {
	Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error)
	Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error)
	Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error)
	Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error
	Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth
}

// PreparedPutAdapter is an optional zero-copy path for adapters that can take
// ownership of BucketMux's durable upload spool. Other adapters continue to
// receive the same io.Reader contract through Adapter.Put.
type PreparedPutAdapter interface {
	PutPrepared(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, upload PreparedUpload) (domain.StoredObject, error)
}

// ObjectLister and BucketDiscoverer are optional capabilities. Adapters that
// cannot enumerate their backing service remain valid storage targets while
// the inventory API returns an explicit unsupported-capability error.
type ObjectLister interface {
	ListObjects(ctx context.Context, account domain.ProviderAccount, bucket, prefix, continuationToken string, limit int) (domain.ProviderObjectPage, error)
}

type BucketDiscoverer interface {
	DiscoverBuckets(ctx context.Context, account domain.ProviderAccount) ([]domain.ProviderBucket, error)
}

// QuotaReporter is implemented only when an adapter can measure the backing
// storage directly. S3-compatible APIs intentionally do not implement it:
// the S3 protocol has no portable account-quota operation.
type QuotaReporter interface {
	Quota(ctx context.Context, account domain.ProviderAccount) (capacityBytes, usedBytes int64, source string, err error)
}

type CapabilityReporter interface {
	Capabilities(account domain.ProviderAccount) domain.ProviderCapabilities
}

type Registry struct {
	adapters map[domain.ProviderKind]Adapter
}

func NewRegistry(adapters ...struct {
	Kind    domain.ProviderKind
	Adapter Adapter
}) *Registry {
	r := &Registry{adapters: map[domain.ProviderKind]Adapter{}}
	for _, entry := range adapters {
		r.adapters[entry.Kind] = entry.Adapter
	}
	return r
}

func (r *Registry) Get(kind domain.ProviderKind) (Adapter, bool) {
	adapter, ok := r.adapters[kind]
	return adapter, ok
}

func Entry(kind domain.ProviderKind, adapter Adapter) struct {
	Kind    domain.ProviderKind
	Adapter Adapter
} {
	return struct {
		Kind    domain.ProviderKind
		Adapter Adapter
	}{Kind: kind, Adapter: adapter}
}
