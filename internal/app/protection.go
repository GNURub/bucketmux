package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
)

var ErrObjectLocked = errors.New("object is protected by retention or legal hold")

type DeleteObjectOptions struct {
	VersionID        string
	BypassGovernance bool
	Permanent        bool
}

type DeleteObjectResult struct {
	VersionID    string
	DeleteMarker bool
}

func (s *Service) GetProtectedObject(ctx context.Context, bucket, key, versionID string) (domain.ObjectRecord, error) {
	if versionID != "" {
		return s.Store.GetObjectVersion(ctx, bucket, key, versionID)
	}
	object, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	if err := s.Store.HydrateObjectAttributes(ctx, &object); err != nil {
		return domain.ObjectRecord{}, err
	}
	return object, nil
}

func (s *Service) UpdateObjectProtection(ctx context.Context, object domain.ObjectRecord, bypassGovernance bool) error {
	bucket, err := s.Store.GetBucket(ctx, object.Bucket)
	if err != nil {
		return err
	}
	if !bucket.ObjectLockEnabled {
		return fmt.Errorf("object lock is not enabled for bucket")
	}
	existing, err := s.GetProtectedObject(ctx, object.Bucket, object.Key, object.VersionID)
	if err != nil {
		return err
	}
	if existing.RetentionMode == "COMPLIANCE" && existing.RetainUntil.After(time.Now().UTC()) && object.RetainUntil.Before(existing.RetainUntil) {
		return ErrObjectLocked
	}
	if existing.RetentionMode == "GOVERNANCE" && existing.RetainUntil.After(time.Now().UTC()) && object.RetainUntil.Before(existing.RetainUntil) && !bypassGovernance {
		return ErrObjectLocked
	}
	existing.RetentionMode = object.RetentionMode
	existing.RetainUntil = object.RetainUntil
	existing.LegalHold = object.LegalHold
	if existing.VersionID != "" {
		if err := s.Store.UpdateObjectVersionProtection(ctx, existing); err != nil {
			return err
		}
	}
	current, currentErr := s.Store.GetObject(ctx, existing.Bucket, existing.Key)
	if currentErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &current)
		if current.VersionID == existing.VersionID {
			return s.Store.PutObjectAttributes(ctx, existing)
		}
	}
	if existing.VersionID == "" {
		return s.Store.PutObjectAttributes(ctx, existing)
	}
	return nil
}

func (s *Service) UpdateObjectTags(ctx context.Context, bucket, key, versionID string, tags map[string]string) error {
	object, err := s.GetProtectedObject(ctx, bucket, key, versionID)
	if err != nil {
		return err
	}
	object.Tags = tags
	if object.VersionID != "" {
		if err := s.Store.UpdateObjectVersionProtection(ctx, object); err != nil {
			return err
		}
	}
	current, currentErr := s.Store.GetObject(ctx, bucket, key)
	if currentErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &current)
		if current.VersionID == object.VersionID {
			return s.Store.PutObjectAttributes(ctx, object)
		}
	}
	if object.VersionID == "" {
		return currentErr
	}
	return nil
}

func (s *Service) DeleteObjectWithOptions(ctx context.Context, bucketName, key string, options DeleteObjectOptions) (DeleteObjectResult, error) {
	bucket, err := s.Store.GetBucket(ctx, bucketName)
	if err != nil {
		return DeleteObjectResult{}, err
	}
	if options.VersionID != "" {
		return s.deleteObjectVersion(ctx, bucket, key, options)
	}
	object, account, err := s.Store.GetObjectWithProvider(ctx, bucketName, key)
	if err != nil {
		return DeleteObjectResult{}, err
	}
	_ = s.Store.HydrateObjectAttributes(ctx, &object)
	if err := validateObjectDeletion(object, options); err != nil {
		return DeleteObjectResult{}, err
	}
	if bucket.VersioningEnabled && !options.Permanent {
		if object.VersionID == "" {
			object.VersionID = "null"
		}
		if err := s.Store.PutObjectVersion(ctx, object); err != nil {
			return DeleteObjectResult{}, err
		}
		markerID := randomIdentifier("v-", 12)
		if err := s.Store.PutObjectVersion(ctx, domain.ObjectRecord{Bucket: bucketName, Key: key, VersionID: markerID, IsDeleteMarker: true, CreatedAt: time.Now().UTC()}); err != nil {
			return DeleteObjectResult{}, err
		}
		s.deleteObjectReplicas(ctx, object)
		if err := s.Store.DeleteObject(ctx, bucketName, key); err != nil {
			return DeleteObjectResult{}, err
		}
		s.dispatchObjectHook(ctx, domain.HookEventObjectDeleted, object)
		return DeleteObjectResult{VersionID: markerID, DeleteMarker: true}, nil
	}
	if bucket.TrashEnabled && !options.Permanent {
		now := time.Now().UTC()
		trash := domain.TrashRecord{ID: randomIdentifier("trash-", 10), Object: object, DeletedAt: now, PurgeAfter: now.Add(time.Duration(bucket.TrashRetentionDays) * 24 * time.Hour)}
		if err := s.Store.PutTrashObject(ctx, trash); err != nil {
			return DeleteObjectResult{}, err
		}
		s.deleteObjectReplicas(ctx, object)
		if err := s.Store.DeleteObject(ctx, bucketName, key); err != nil {
			return DeleteObjectResult{}, err
		}
		s.dispatchObjectHook(ctx, domain.HookEventObjectDeleted, object)
		return DeleteObjectResult{}, nil
	}
	account, adapter, err := s.providerForReplica(ctx, account)
	if err != nil {
		return DeleteObjectResult{}, err
	}
	if err := adapter.Delete(ctx, account, object); err != nil {
		return DeleteObjectResult{}, err
	}
	s.deleteObjectReplicas(ctx, object)
	if err := s.Store.DeleteObject(ctx, bucketName, key); err != nil {
		return DeleteObjectResult{}, err
	}
	_ = s.Store.AddProviderUsage(ctx, object.ProviderAccountID, -object.Size)
	s.dispatchObjectHook(ctx, domain.HookEventObjectDeleted, object)
	return DeleteObjectResult{}, nil
}

func validateObjectDeletion(object domain.ObjectRecord, options DeleteObjectOptions) error {
	if object.LegalHold {
		return ErrObjectLocked
	}
	if object.RetainUntil.After(time.Now().UTC()) && (object.RetentionMode == "COMPLIANCE" || !options.BypassGovernance) {
		return ErrObjectLocked
	}
	return nil
}

func (s *Service) deleteObjectVersion(ctx context.Context, bucket domain.Bucket, key string, options DeleteObjectOptions) (DeleteObjectResult, error) {
	object, err := s.Store.GetObjectVersion(ctx, bucket.Name, key, options.VersionID)
	if err != nil {
		return DeleteObjectResult{}, err
	}
	if object.IsDeleteMarker {
		if err := s.Store.DeleteObjectVersion(ctx, bucket.Name, key, object.VersionID); err != nil {
			return DeleteObjectResult{}, err
		}
		if err := s.restoreLatestObjectVersion(ctx, bucket, key); err != nil {
			return DeleteObjectResult{}, err
		}
		return DeleteObjectResult{VersionID: object.VersionID, DeleteMarker: true}, nil
	}
	if err := validateObjectDeletion(object, options); err != nil {
		return DeleteObjectResult{}, err
	}
	account, adapter, err := s.providerForObject(ctx, object)
	if err != nil {
		return DeleteObjectResult{}, err
	}
	if err := adapter.Delete(ctx, account, object); err != nil {
		return DeleteObjectResult{}, err
	}
	if err := s.Store.DeleteObjectVersion(ctx, bucket.Name, key, object.VersionID); err != nil {
		return DeleteObjectResult{}, err
	}
	if current, currentErr := s.Store.GetObject(ctx, bucket.Name, key); currentErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &current)
		if current.VersionID == object.VersionID {
			s.deleteObjectReplicas(ctx, current)
			if err := s.Store.DeleteObject(ctx, bucket.Name, key); err != nil {
				return DeleteObjectResult{}, err
			}
			versions, err := s.Store.ListObjectVersions(ctx, bucket.Name, key, 1000)
			if err != nil {
				return DeleteObjectResult{}, err
			}
			for _, previous := range versions {
				if previous.Key != key {
					continue
				}
				if previous.IsDeleteMarker {
					break
				}
				if err := s.restoreVersionAsCurrent(ctx, bucket, previous); err != nil {
					return DeleteObjectResult{}, err
				}
				break
			}
		}
	}
	_ = s.Store.AddProviderUsage(ctx, object.ProviderAccountID, -object.Size)
	return DeleteObjectResult{VersionID: object.VersionID}, nil
}

func (s *Service) restoreLatestObjectVersion(ctx context.Context, bucket domain.Bucket, key string) error {
	versions, err := s.Store.ListObjectVersions(ctx, bucket.Name, key, 1000)
	if err != nil {
		return err
	}
	for _, previous := range versions {
		if previous.Key != key {
			continue
		}
		if previous.IsDeleteMarker {
			return nil
		}
		return s.restoreVersionAsCurrent(ctx, bucket, previous)
	}
	return nil
}

func (s *Service) restoreVersionAsCurrent(ctx context.Context, bucket domain.Bucket, object domain.ObjectRecord) error {
	if err := s.Store.PutObject(ctx, object); err != nil {
		return err
	}
	if err := s.Store.PutObjectAttributes(ctx, object); err != nil {
		return err
	}
	targets := replicaTargets(bucket, object.ProviderAccountID)
	if len(targets) == 0 {
		return nil
	}
	object.ReplicaStatus = "pending"
	if err := s.Store.PutObject(ctx, object); err != nil {
		return err
	}
	return s.enqueueObjectReplicas(ctx, object, targets)
}

func (s *Service) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (io.ReadCloser, domain.ObjectRecord, error) {
	object, err := s.Store.GetObjectVersion(ctx, bucket, key, versionID)
	if err != nil {
		return nil, domain.ObjectRecord{}, err
	}
	if object.IsDeleteMarker {
		return nil, object, store.ErrNotFound
	}
	account, adapter, err := s.providerForObject(ctx, object)
	if err != nil {
		return nil, object, err
	}
	body, served, err := adapter.Get(ctx, account, object)
	return body, served, err
}

func (s *Service) HeadObjectVersion(ctx context.Context, bucket, key, versionID string) (domain.ObjectRecord, error) {
	object, err := s.Store.GetObjectVersion(ctx, bucket, key, versionID)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	if object.IsDeleteMarker {
		return object, store.ErrNotFound
	}
	account, adapter, err := s.providerForObject(ctx, object)
	if err != nil {
		return object, err
	}
	return adapter.Head(ctx, account, object)
}

func (s *Service) RestoreTrashObject(ctx context.Context, id string) (domain.ObjectRecord, error) {
	trash, err := s.Store.GetTrashObject(ctx, id)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	if _, err := s.Store.GetObject(ctx, trash.Object.Bucket, trash.Object.Key); err == nil {
		return domain.ObjectRecord{}, fmt.Errorf("object already exists")
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.ObjectRecord{}, err
	}
	if err := s.Store.PutObject(ctx, trash.Object); err != nil {
		return domain.ObjectRecord{}, err
	}
	if err := s.Store.PutObjectAttributes(ctx, trash.Object); err != nil {
		return domain.ObjectRecord{}, err
	}
	if err := s.Store.DeleteTrashObject(ctx, id); err != nil {
		return domain.ObjectRecord{}, err
	}
	return trash.Object, nil
}

func (s *Service) PurgeTrashObject(ctx context.Context, id string) error {
	trash, err := s.Store.GetTrashObject(ctx, id)
	if err != nil {
		return err
	}
	account, adapter, err := s.providerForObject(ctx, trash.Object)
	if err != nil {
		return err
	}
	if err := adapter.Delete(ctx, account, trash.Object); err != nil {
		return err
	}
	if err := s.Store.DeleteTrashObject(ctx, id); err != nil {
		return err
	}
	return s.Store.AddProviderUsage(ctx, trash.Object.ProviderAccountID, -trash.Object.Size)
}

func (s *Service) cleanupOverwrittenObject(ctx context.Context, previous, current domain.ObjectRecord) error {
	_ = s.Store.AddProviderUsage(ctx, previous.ProviderAccountID, -previous.Size)
	if previous.ProviderAccountID == current.ProviderAccountID && previous.RemoteBucket == current.RemoteBucket && previous.RemoteKey == current.RemoteKey {
		return nil
	}
	account, adapter, err := s.providerForObject(ctx, previous)
	if err != nil {
		return err
	}
	return adapter.Delete(ctx, account, previous)
}
