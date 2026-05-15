package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const (
	replicaStatusPending   = "pending"
	replicaStatusRunning   = "running"
	replicaStatusSucceeded = "succeeded"
	replicaStatusFailed    = "failed"
)

func (s *Service) enqueueObjectReplicas(ctx context.Context, primary domain.ObjectRecord, targets []string) error {
	for _, providerID := range targets {
		if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: primary.Bucket, Key: primary.Key, ProviderAccountID: providerID, Size: primary.Size, Status: replicaStatusPending}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) StartReplicationWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		lease, ok := s.tryWorkerLease(ctx, "replication")
		if ok {
			replica, claimed, err := s.Store.ClaimNextObjectReplica(ctx)
			if err == nil && claimed {
				_ = s.RunObjectReplication(ctx, replica.Bucket, replica.Key, replica.ProviderAccountID)
				_ = lease.Release(ctx)
				continue
			}
			_ = lease.Release(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) RunObjectReplication(ctx context.Context, bucket, key, providerID string) error {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: bucket, Key: key, ProviderAccountID: providerID, Status: replicaStatusFailed, Error: "source object not found"}); err != nil {
			return err
		}
		return s.refreshObjectReplicaStatus(ctx, bucket, key)
	}
	account, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	if !account.Enabled {
		return s.failObjectReplica(ctx, obj, providerID, fmt.Errorf("provider is disabled"))
	}
	if account.CapacityBytes > 0 && account.UsedBytes+obj.Size > account.CapacityBytes {
		return s.failObjectReplica(ctx, obj, providerID, fmt.Errorf("provider has insufficient capacity"))
	}
	body, _, err := s.GetObject(ctx, obj.Bucket, obj.Key)
	if err != nil {
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	defer body.Close()
	stored, err := s.putOnProvider(ctx, account, domain.PutObjectInput{Bucket: obj.Bucket, Key: obj.Key, Size: obj.Size, ContentType: obj.ContentType}, body)
	if err != nil {
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ETag: stored.ETag, Status: replicaStatusSucceeded}); err != nil {
		return err
	}
	_ = s.Store.AddProviderUsage(ctx, providerID, stored.Size)
	return s.refreshObjectReplicaStatus(ctx, obj.Bucket, obj.Key)
}

func (s *Service) failObjectReplica(ctx context.Context, obj domain.ObjectRecord, providerID string, cause error) error {
	_ = s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, Status: replicaStatusFailed, Error: cause.Error()})
	_ = s.refreshObjectReplicaStatus(ctx, obj.Bucket, obj.Key)
	return cause
}

func (s *Service) refreshObjectReplicaStatus(ctx context.Context, bucket, key string) error {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	replicas, err := s.Store.ListObjectReplicas(ctx, bucket, key)
	if err != nil {
		return err
	}
	if len(replicas) == 0 {
		obj.ReplicaStatus = "none"
		return s.Store.PutObject(ctx, obj)
	}
	succeeded := 0
	failed := 0
	pending := 0
	for _, replica := range replicas {
		switch replica.Status {
		case replicaStatusSucceeded:
			succeeded++
		case replicaStatusFailed:
			failed++
		default:
			pending++
		}
	}
	switch {
	case succeeded == len(replicas):
		obj.ReplicaStatus = "completed"
	case succeeded > 0:
		obj.ReplicaStatus = "partial"
	case failed == len(replicas):
		obj.ReplicaStatus = "failed"
	case pending > 0:
		obj.ReplicaStatus = "pending"
	default:
		obj.ReplicaStatus = "pending"
	}
	return s.Store.PutObject(ctx, obj)
}
