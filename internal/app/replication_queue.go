package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
)

const (
	replicaStatusPending   = "pending"
	replicaStatusRunning   = "running"
	replicaStatusSucceeded = "succeeded"
	replicaStatusFailed    = "failed"
)

const replicationWorkerInterval = 2 * time.Second
const replicationHeartbeatInterval = 30 * time.Second
const replicationWorkStaleAfter = 15 * time.Minute

func (s *Service) enqueueObjectReplicas(ctx context.Context, primary domain.ObjectRecord, targets []string) error {
	for _, providerID := range targets {
		if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: primary.Bucket, Key: primary.Key, ProviderAccountID: providerID, Size: primary.Size, ChecksumSHA256: primary.ChecksumSHA256, Status: replicaStatusPending, MaxAttempts: 5}); err != nil {
			return err
		}
	}
	signalWorker(s.replicationWake)
	return nil
}

func (s *Service) StartReplicationWorker(ctx context.Context) {
	s.runDurableWorker(ctx, durableWorker{
		name:              "replication",
		interval:          replicationWorkerInterval,
		heartbeatInterval: replicationHeartbeatInterval,
		staleAfter:        replicationWorkStaleAfter,
		wake:              s.replicationWake,
		recover: func(ctx context.Context, cutoff time.Time) error {
			_, err := s.Store.RecoverStaleObjectReplicas(ctx, cutoff)
			return err
		},
		claim: func(ctx context.Context) (durableWorkItem, bool, error) {
			replica, claimed, err := s.Store.ClaimNextObjectReplica(ctx)
			if err != nil || !claimed {
				return durableWorkItem{}, claimed, err
			}
			return durableWorkItem{
				run: func(ctx context.Context) error {
					return s.RunObjectReplication(ctx, replica.Bucket, replica.Key, replica.ProviderAccountID)
				},
				heartbeat: func(ctx context.Context) error {
					return s.Store.TouchObjectReplica(ctx, replica.Bucket, replica.Key, replica.ProviderAccountID)
				},
			}, true, nil
		},
	})
}

func (s *Service) RunObjectReplication(ctx context.Context, bucket, key, providerID string) error {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: bucket, Key: key, ProviderAccountID: providerID, Status: replicaStatusFailed, Error: "source object not found", Attempts: 1, MaxAttempts: 1}); err != nil {
			return err
		}
		return s.refreshObjectReplicaStatus(ctx, bucket, key)
	}
	account, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	if !account.Enabled {
		return s.failObjectReplicaPermanently(ctx, obj, providerID, fmt.Errorf("provider is disabled"))
	}
	reservation := domain.ProviderReservation{ID: randomIdentifier("replica-res-", 10), ProviderAccountID: providerID, Bytes: obj.Size, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	margin := intSetting(account.Settings, "quota_margin_bytes")
	if minFree := intSetting(account.Settings, "min_free_bytes"); minFree > margin {
		margin = minFree
	}
	reserved, err := s.Store.ReserveProviderCapacity(ctx, reservation, margin, intSetting(account.Settings, "monthly_upload_quota_bytes"), time.Now().UTC().Format("2006-01"))
	if err != nil {
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	if !reserved {
		return s.failObjectReplica(ctx, obj, providerID, &provider.Error{Op: "reserve replica capacity", Kind: provider.FailureQuota, Err: fmt.Errorf("provider has insufficient quota")})
	}
	body, _, err := s.GetObject(ctx, obj.Bucket, obj.Key)
	if err != nil {
		_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	defer func() { _ = body.Close() }()
	stored, err := s.putOnProvider(ctx, account, domain.PutObjectInput{Bucket: obj.Bucket, Key: obj.Key, Size: obj.Size, ContentType: obj.ContentType, ChecksumSHA256: obj.ChecksumSHA256}, body)
	if err != nil {
		_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
		s.recordProviderWriteFailure(ctx, account, err)
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	if stored.ChecksumSHA256 == "" {
		stored.ChecksumSHA256 = obj.ChecksumSHA256
	}
	decrypted, adapter, err := s.providerForReplica(ctx, account)
	if err != nil {
		_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	replicaRecord := domain.ObjectRecord{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ETag: stored.ETag, ChecksumSHA256: stored.ChecksumSHA256}
	if verifyErr := verifyReplica(ctx, adapter, decrypted, replicaRecord, obj.Size, obj.ChecksumSHA256); verifyErr != nil {
		_ = adapter.Delete(ctx, decrypted, replicaRecord)
		_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
		return s.failObjectReplica(ctx, obj, providerID, verifyErr)
	}
	if err := s.Store.CommitProviderReservation(ctx, reservation.ID, stored.Size); err != nil {
		_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
		return s.failObjectReplica(ctx, obj, providerID, err)
	}
	attempts := 1
	maxAttempts := 5
	if replicas, listErr := s.Store.ListObjectReplicas(ctx, obj.Bucket, obj.Key); listErr == nil {
		for _, current := range replicas {
			if current.ProviderAccountID == providerID {
				attempts = max(current.Attempts, 1)
				maxAttempts = max(current.MaxAttempts, 1)
				break
			}
		}
	}
	if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ETag: stored.ETag, ChecksumSHA256: stored.ChecksumSHA256, Status: replicaStatusSucceeded, Attempts: attempts, MaxAttempts: maxAttempts}); err != nil {
		return err
	}
	return s.refreshObjectReplicaStatus(ctx, obj.Bucket, obj.Key)
}

func verifyReplica(ctx context.Context, adapter provider.Adapter, account domain.ProviderAccount, replica domain.ObjectRecord, expectedSize int64, expectedChecksum string) error {
	head, err := adapter.Head(ctx, account, replica)
	if err != nil {
		return fmt.Errorf("head replica for verification: %w", err)
	}
	if head.Size != expectedSize {
		return fmt.Errorf("replica verification mismatch: size=%d want=%d", head.Size, expectedSize)
	}
	if expectedChecksum == "" {
		return nil
	}
	checksum := head.ChecksumSHA256
	if checksum == "" {
		body, _, err := adapter.Get(ctx, account, replica)
		if err != nil {
			return fmt.Errorf("read replica for checksum verification: %w", err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, body)
		closeErr := body.Close()
		if copyErr != nil {
			return fmt.Errorf("hash replica for verification: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close replica after verification: %w", closeErr)
		}
		checksum = hex.EncodeToString(hash.Sum(nil))
	}
	if !strings.EqualFold(checksum, expectedChecksum) {
		return fmt.Errorf("replica checksum mismatch: checksum=%q want=%q", checksum, expectedChecksum)
	}
	return nil
}

func (s *Service) failObjectReplica(ctx context.Context, obj domain.ObjectRecord, providerID string, cause error) error {
	replica := domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, Size: obj.Size, ChecksumSHA256: obj.ChecksumSHA256, MaxAttempts: 5}
	if replicas, err := s.Store.ListObjectReplicas(ctx, obj.Bucket, obj.Key); err == nil {
		for _, current := range replicas {
			if current.ProviderAccountID == providerID {
				replica = current
				break
			}
		}
	}
	if replica.Attempts == 0 {
		replica.Attempts = 1
	}
	if replica.MaxAttempts <= 0 {
		replica.MaxAttempts = 5
	}
	replica.Error = cause.Error()
	if replica.Attempts < replica.MaxAttempts {
		replica.Status = replicaStatusPending
		delay := time.Duration(1<<min(replica.Attempts, 8)) * time.Second
		replica.NextAttemptAt = time.Now().UTC().Add(delay)
	} else {
		replica.Status = replicaStatusFailed
		s.raiseAlert(ctx, domain.Alert{DedupeKey: "replica:" + obj.Bucket + ":" + obj.Key + ":" + providerID, Type: domain.AlertTypeReplicaFailed, Severity: domain.AlertSeverityCritical, ProviderAccountID: providerID, Bucket: obj.Bucket, Key: obj.Key, Message: cause.Error()})
	}
	_ = s.Store.UpsertObjectReplica(ctx, replica)
	_ = s.refreshObjectReplicaStatus(ctx, obj.Bucket, obj.Key)
	return cause
}

func (s *Service) failObjectReplicaPermanently(ctx context.Context, obj domain.ObjectRecord, providerID string, cause error) error {
	_ = s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: providerID, Size: obj.Size, ChecksumSHA256: obj.ChecksumSHA256, Status: replicaStatusFailed, Error: cause.Error(), Attempts: 1, MaxAttempts: 1})
	s.raiseAlert(ctx, domain.Alert{DedupeKey: "replica:" + obj.Bucket + ":" + obj.Key + ":" + providerID, Type: domain.AlertTypeReplicaFailed, Severity: domain.AlertSeverityCritical, ProviderAccountID: providerID, Bucket: obj.Bucket, Key: obj.Key, Message: cause.Error()})
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
