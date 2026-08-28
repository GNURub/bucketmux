package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
	"github.com/gnurub/bucketmux/internal/store"
)

var ErrProviderCapabilityUnsupported = errors.New("provider capability is not supported")

const inventoryWorkerInterval = 2 * time.Second
const inventoryHeartbeatInterval = 30 * time.Second
const inventoryWorkStaleAfter = 15 * time.Minute

type CreateInventoryJobInput struct {
	ProviderAccountID string
	Bucket            string
	RemoteBucket      string
	Prefix            string
	Mode              string
}

func (s *Service) TestProviderConnection(ctx context.Context, providerID string) (domain.ProviderHealth, error) {
	account, err := s.Store.GetProvider(ctx, strings.TrimSpace(providerID))
	if err != nil {
		return domain.ProviderHealth{}, err
	}
	validation, err := s.ValidateProvider(ctx, account)
	if err != nil {
		return domain.ProviderHealth{}, err
	}
	s.recordProviderHealth(ctx, account, validation.Health)
	return validation.Health, nil
}

func (s *Service) DiscoverProviderBuckets(ctx context.Context, providerID string) ([]domain.ProviderBucket, error) {
	account, adapter, err := s.providerAccountAndAdapter(ctx, providerID)
	if err != nil {
		return nil, err
	}
	discoverer, ok := adapter.(provider.BucketDiscoverer)
	if !ok {
		return nil, fmt.Errorf("%w: %s does not expose bucket discovery", ErrProviderCapabilityUnsupported, account.Kind)
	}
	return discoverer.DiscoverBuckets(ctx, account)
}

func (s *Service) CreateInventoryJob(ctx context.Context, input CreateInventoryJobInput) (domain.InventoryJob, error) {
	job := domain.InventoryJob{
		ID:                newInventoryID(),
		ProviderAccountID: strings.TrimSpace(input.ProviderAccountID),
		Bucket:            strings.TrimSpace(input.Bucket),
		RemoteBucket:      strings.TrimSpace(input.RemoteBucket),
		Prefix:            strings.TrimLeft(strings.TrimSpace(input.Prefix), "/"),
		Mode:              strings.TrimSpace(input.Mode),
		Status:            domain.InventoryStatusPending,
	}
	if job.Mode == "" {
		job.Mode = domain.InventoryModeImport
	}
	if job.ProviderAccountID == "" || job.Bucket == "" {
		return domain.InventoryJob{}, fmt.Errorf("provider_account_id and bucket are required")
	}
	if job.Mode != domain.InventoryModeImport && job.Mode != domain.InventoryModeReconcile {
		return domain.InventoryJob{}, fmt.Errorf("mode must be import or reconcile")
	}
	account, adapter, err := s.providerAccountAndAdapter(ctx, job.ProviderAccountID)
	if err != nil {
		return domain.InventoryJob{}, err
	}
	if _, ok := adapter.(provider.ObjectLister); !ok {
		return domain.InventoryJob{}, fmt.Errorf("%w: %s does not expose object inventory", ErrProviderCapabilityUnsupported, account.Kind)
	}
	if job.RemoteBucket == "" {
		job.RemoteBucket = account.Bucket
	}
	if job.RemoteBucket == "" {
		return domain.InventoryJob{}, fmt.Errorf("remote_bucket is required when the provider has no configured bucket")
	}
	if _, err := s.ensureBucket(ctx, job.Bucket); err != nil {
		return domain.InventoryJob{}, err
	}
	if err := s.Store.CreateInventoryJob(ctx, job); err != nil {
		return domain.InventoryJob{}, err
	}
	signalWorker(s.inventoryWake)
	return s.Store.GetInventoryJob(ctx, job.ID)
}

func (s *Service) StartInventoryWorker(ctx context.Context) {
	s.runDurableWorker(ctx, durableWorker{
		name:              "inventory",
		interval:          inventoryWorkerInterval,
		heartbeatInterval: inventoryHeartbeatInterval,
		staleAfter:        inventoryWorkStaleAfter,
		wake:              s.inventoryWake,
		recover: func(ctx context.Context, cutoff time.Time) error {
			_, err := s.Store.RecoverStaleInventoryJobs(ctx, cutoff)
			return err
		},
		claim: func(ctx context.Context) (durableWorkItem, bool, error) {
			job, claimed, err := s.Store.ClaimNextInventoryJob(ctx)
			if err != nil || !claimed {
				return durableWorkItem{}, claimed, err
			}
			return durableWorkItem{
				run:       func(ctx context.Context) error { return s.RunInventoryJob(ctx, job.ID) },
				heartbeat: func(ctx context.Context) error { return s.Store.TouchInventoryJob(ctx, job.ID) },
			}, true, nil
		},
	})
}

func (s *Service) RunInventoryJob(ctx context.Context, id string) error {
	job, err := s.Store.GetInventoryJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == domain.InventoryStatusCompleted || job.Status == domain.InventoryStatusFailed {
		return nil
	}
	job.Status = domain.InventoryStatusRunning
	if err := s.Store.UpdateInventoryJob(ctx, job); err != nil {
		return err
	}
	account, adapter, err := s.providerAccountAndAdapter(ctx, job.ProviderAccountID)
	if err != nil {
		return s.failInventoryJob(ctx, &job, err)
	}
	if job.RemoteBucket == "" {
		job.RemoteBucket = account.Bucket
	}
	if job.RemoteBucket == "" {
		return s.failInventoryJob(ctx, &job, fmt.Errorf("remote bucket is not configured"))
	}
	lister, ok := adapter.(provider.ObjectLister)
	if !ok {
		return s.failInventoryJob(ctx, &job, fmt.Errorf("%w: %s", ErrProviderCapabilityUnsupported, account.Kind))
	}
	seen := map[string]struct{}{}
	token := ""
	for {
		page, err := lister.ListObjects(ctx, account, job.RemoteBucket, job.Prefix, token, 1000)
		if err != nil {
			return s.failInventoryJob(ctx, &job, err)
		}
		for _, remote := range page.Objects {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			job.DiscoveredObjects++
			seen[remote.Key] = struct{}{}
			existing, getErr := s.Store.GetObject(ctx, job.Bucket, remote.Key)
			if getErr == nil && existing.ProviderAccountID != account.ID {
				continue
			}
			if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
				return s.failInventoryJob(ctx, &job, getErr)
			}
			createdAt := remote.LastModified
			if createdAt.IsZero() {
				createdAt = time.Now().UTC()
			}
			if err := s.Store.PutObject(ctx, domain.ObjectRecord{Bucket: job.Bucket, Key: remote.Key, ProviderAccountID: account.ID, RemoteBucket: job.RemoteBucket, RemoteKey: remote.Key, Size: remote.Size, ContentType: remote.ContentType, ETag: remote.ETag, ReplicaStatus: "none", CreatedAt: createdAt}); err != nil {
				return s.failInventoryJob(ctx, &job, err)
			}
			if getErr != nil {
				job.ImportedObjects++
				_ = s.Store.AddProviderUsage(ctx, account.ID, remote.Size)
			}
		}
		_ = s.Store.UpdateInventoryJob(ctx, job)
		if page.NextContinuationToken == "" || page.NextContinuationToken == token {
			break
		}
		token = page.NextContinuationToken
	}
	if job.Mode == domain.InventoryModeReconcile {
		startAfter := ""
		for {
			indexed, err := s.Store.ListObjectsAfter(ctx, job.Bucket, job.Prefix, startAfter, 1000)
			if err != nil {
				return s.failInventoryJob(ctx, &job, err)
			}
			for _, object := range indexed {
				if object.ProviderAccountID == account.ID {
					if _, ok := seen[object.RemoteKey]; !ok {
						job.MissingObjects++
					}
				}
			}
			if len(indexed) < 1000 {
				break
			}
			startAfter = indexed[len(indexed)-1].Key
		}
	}
	job.Status = domain.InventoryStatusCompleted
	job.FinishedAt = time.Now().UTC()
	job.LastError = ""
	return s.Store.UpdateInventoryJob(ctx, job)
}

func (s *Service) failInventoryJob(ctx context.Context, job *domain.InventoryJob, cause error) error {
	job.Status = domain.InventoryStatusFailed
	job.LastError = cause.Error()
	job.FinishedAt = time.Now().UTC()
	_ = s.Store.UpdateInventoryJob(ctx, *job)
	return cause
}

func (s *Service) providerAccountAndAdapter(ctx context.Context, providerID string) (domain.ProviderAccount, provider.Adapter, error) {
	account, err := s.Store.GetProvider(ctx, strings.TrimSpace(providerID))
	if err != nil {
		return domain.ProviderAccount{}, nil, err
	}
	if !account.Enabled {
		return domain.ProviderAccount{}, nil, fmt.Errorf("provider is disabled")
	}
	return s.providerForReplica(ctx, account)
}

func newInventoryID() string {
	buffer := make([]byte, 8)
	_, _ = rand.Read(buffer)
	return "inv-" + hex.EncodeToString(buffer)
}
