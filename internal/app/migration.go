package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const MigrationMoveConfirmationPhrase = "Migrar permanentemente"

type CreateMigrationJobInput struct {
	Bucket           string
	Prefix           string
	SourceProviderID string
	TargetProviderID string
	Mode             string
	Confirm          string
}

func (s *Service) CreateMigrationJob(ctx context.Context, input CreateMigrationJobInput) (domain.MigrationJob, error) {
	job := domain.MigrationJob{
		ID:               newMigrationID(),
		Bucket:           strings.TrimSpace(input.Bucket),
		Prefix:           strings.Trim(strings.TrimSpace(input.Prefix), "/"),
		SourceProviderID: strings.TrimSpace(input.SourceProviderID),
		TargetProviderID: strings.TrimSpace(input.TargetProviderID),
		Mode:             strings.TrimSpace(input.Mode),
		Status:           domain.MigrationStatusPending,
	}
	if job.Mode == "" {
		job.Mode = domain.MigrationModeCopy
	}
	if job.Bucket == "" || job.SourceProviderID == "" || job.TargetProviderID == "" {
		return domain.MigrationJob{}, fmt.Errorf("bucket, source_provider_id and target_provider_id are required")
	}
	if job.SourceProviderID == job.TargetProviderID {
		return domain.MigrationJob{}, fmt.Errorf("source and target providers must be different")
	}
	if job.Mode != domain.MigrationModeCopy && job.Mode != domain.MigrationModeMove {
		return domain.MigrationJob{}, fmt.Errorf("mode must be copy or move")
	}
	if job.Mode == domain.MigrationModeMove && input.Confirm != MigrationMoveConfirmationPhrase {
		return domain.MigrationJob{}, fmt.Errorf("confirmation must exactly match %q", MigrationMoveConfirmationPhrase)
	}
	if _, err := s.Store.GetBucket(ctx, job.Bucket); err != nil {
		return domain.MigrationJob{}, fmt.Errorf("bucket not found: %w", err)
	}
	if source, err := s.Store.GetProvider(ctx, job.SourceProviderID); err != nil {
		return domain.MigrationJob{}, fmt.Errorf("source provider not found: %w", err)
	} else if !source.Enabled {
		return domain.MigrationJob{}, fmt.Errorf("source provider is disabled")
	}
	if target, err := s.Store.GetProvider(ctx, job.TargetProviderID); err != nil {
		return domain.MigrationJob{}, fmt.Errorf("target provider not found: %w", err)
	} else if !target.Enabled {
		return domain.MigrationJob{}, fmt.Errorf("target provider is disabled")
	}
	if err := s.Store.CreateMigrationJob(ctx, job); err != nil {
		return domain.MigrationJob{}, err
	}
	return s.Store.GetMigrationJob(ctx, job.ID)
}

func (s *Service) StartMigrationWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		lease, leased := s.tryWorkerLease(ctx, "migration")
		if leased {
			job, ok, err := s.Store.ClaimNextMigrationJob(ctx)
			if err == nil && ok {
				_ = s.RunMigrationJob(ctx, job.ID)
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

func (s *Service) RunMigrationJob(ctx context.Context, id string) error {
	job, err := s.Store.GetMigrationJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == domain.MigrationStatusCompleted || job.Status == domain.MigrationStatusFailed {
		return nil
	}
	job.Status = domain.MigrationStatusRunning
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now().UTC()
	}
	if err := s.Store.UpdateMigrationJob(ctx, job); err != nil {
		return err
	}
	objects, err := s.migrationObjects(ctx, job)
	if err != nil {
		job.Status = domain.MigrationStatusFailed
		job.LastError = err.Error()
		job.FinishedAt = time.Now().UTC()
		_ = s.Store.UpdateMigrationJob(ctx, job)
		return err
	}
	job.TotalObjects = len(objects)
	if err := s.Store.UpdateMigrationJob(ctx, job); err != nil {
		return err
	}
	for _, obj := range objects {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		job.CurrentKey = obj.Key
		if err := s.Store.UpdateMigrationJob(ctx, job); err != nil {
			return err
		}
		err := s.migrateObjectToProvider(ctx, obj, job.TargetProviderID, job.Mode)
		job.ProcessedObjects++
		if err != nil {
			job.FailedObjects++
			job.LastError = fmt.Sprintf("%s: %v", obj.Key, err)
		} else {
			job.SucceededObjects++
		}
		if err := s.Store.UpdateMigrationJob(ctx, job); err != nil {
			return err
		}
	}
	job.CurrentKey = ""
	job.FinishedAt = time.Now().UTC()
	if job.FailedObjects > 0 {
		job.Status = domain.MigrationStatusFailed
	} else {
		job.Status = domain.MigrationStatusCompleted
	}
	return s.Store.UpdateMigrationJob(ctx, job)
}

func (s *Service) migrationObjects(ctx context.Context, job domain.MigrationJob) ([]domain.ObjectRecord, error) {
	var out []domain.ObjectRecord
	startAfter := ""
	for {
		objects, err := s.Store.ListObjectsAfter(ctx, job.Bucket, job.Prefix, startAfter, 1000)
		if err != nil {
			return nil, err
		}
		if len(objects) == 0 {
			break
		}
		for _, obj := range objects {
			if obj.ProviderAccountID == job.SourceProviderID {
				out = append(out, obj)
			}
		}
		if len(objects) < 1000 {
			break
		}
		startAfter = objects[len(objects)-1].Key
	}
	return out, nil
}

func (s *Service) migrateObjectToProvider(ctx context.Context, obj domain.ObjectRecord, targetProviderID, mode string) error {
	sourceAccount, sourceAdapter, err := s.providerForObject(ctx, obj)
	if err != nil {
		return err
	}
	targetAccount, err := s.Store.GetProvider(ctx, targetProviderID)
	if err != nil {
		return err
	}
	if !targetAccount.Enabled {
		return fmt.Errorf("target provider is disabled")
	}
	if targetAccount.CapacityBytes > 0 && targetAccount.UsedBytes+obj.Size > targetAccount.CapacityBytes {
		return fmt.Errorf("target provider has insufficient capacity")
	}
	body, _, err := sourceAdapter.Get(ctx, sourceAccount, obj)
	if err != nil {
		return err
	}
	defer body.Close()
	stored, err := s.putOnProvider(ctx, targetAccount, domain.PutObjectInput{Bucket: obj.Bucket, Key: obj.Key, Size: obj.Size, ContentType: obj.ContentType}, body)
	if err != nil {
		return err
	}
	if mode == domain.MigrationModeCopy {
		_ = s.subtractExistingReplicaUsage(ctx, obj.Bucket, obj.Key, targetProviderID)
		if err := s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: obj.Bucket, Key: obj.Key, ProviderAccountID: targetProviderID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ETag: stored.ETag, Status: "succeeded"}); err != nil {
			return err
		}
		return s.Store.AddProviderUsage(ctx, targetProviderID, stored.Size)
	}
	migrated := obj
	migrated.ProviderAccountID = targetProviderID
	migrated.RemoteBucket = stored.RemoteBucket
	migrated.RemoteKey = stored.RemoteKey
	migrated.Size = stored.Size
	migrated.ContentType = stored.ContentType
	migrated.ETag = stored.ETag
	migrated.ChecksumSHA256 = stored.ChecksumSHA256
	if err := s.Store.PutObject(ctx, migrated); err != nil {
		return err
	}
	_ = s.Store.AddProviderUsage(ctx, targetProviderID, stored.Size)
	if err := sourceAdapter.Delete(ctx, sourceAccount, obj); err == nil {
		_ = s.Store.AddProviderUsage(ctx, sourceAccount.ID, -obj.Size)
	}
	return nil
}

func (s *Service) subtractExistingReplicaUsage(ctx context.Context, bucket, key, providerID string) error {
	replicas, err := s.Store.ListObjectReplicas(ctx, bucket, key)
	if err != nil {
		return err
	}
	for _, replica := range replicas {
		if replica.ProviderAccountID == providerID && replica.Size > 0 && replica.Status == "succeeded" {
			return s.Store.AddProviderUsage(ctx, providerID, -replica.Size)
		}
	}
	return nil
}

func newMigrationID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("mig-%d", time.Now().UnixNano())
	}
	return "mig-" + hex.EncodeToString(bytes[:])
}
