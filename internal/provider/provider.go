package provider

import (
	"context"
	"io"

	"github.com/gnurub/bucketmux/internal/domain"
)

type Adapter interface {
	Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error)
	Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error)
	Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error)
	Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error
	Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth
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
