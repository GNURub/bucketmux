package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) PutObjectVersion(ctx context.Context, object domain.ObjectRecord) error {
	metadata, _ := json.Marshal(nonNilStringMap(object.Metadata))
	tags, _ := json.Marshal(nonNilStringMap(object.Tags))
	created := object.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO object_versions (version_id, bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, metadata_json, tags_json, retention_mode, retain_until, legal_hold, is_delete_marker, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(bucket, key, version_id) DO NOTHING`, object.VersionID, object.Bucket, object.Key, object.ProviderAccountID, object.RemoteBucket, object.RemoteKey, object.Size, object.ContentType, object.ETag, object.ChecksumSHA256, string(metadata), string(tags), object.RetentionMode, formatOptionalTime(object.RetainUntil), boolToInt(object.LegalHold), boolToInt(object.IsDeleteMarker), created.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (domain.ObjectRecord, error) {
	object, err := scanObjectVersion(s.queryRow(ctx, objectVersionSelect+` WHERE bucket = ? AND key = ? AND version_id = ?`, bucket, key, versionID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ObjectRecord{}, ErrNotFound
	}
	return object, err
}

func (s *Store) ListObjectVersions(ctx context.Context, bucket, prefix string, limit int) ([]domain.ObjectRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.query(ctx, objectVersionSelect+` WHERE bucket = ? AND substr(key, 1, length(?)) = ? ORDER BY key ASC, created_at DESC LIMIT ?`, bucket, prefix, prefix, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var versions []domain.ObjectRecord
	for rows.Next() {
		object, err := scanObjectVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, object)
	}
	return versions, rows.Err()
}

func (s *Store) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) error {
	_, err := s.exec(ctx, `DELETE FROM object_versions WHERE bucket = ? AND key = ? AND version_id = ?`, bucket, key, versionID)
	return err
}

func (s *Store) UpdateObjectVersionProtection(ctx context.Context, object domain.ObjectRecord) error {
	metadata, _ := json.Marshal(nonNilStringMap(object.Metadata))
	tags, _ := json.Marshal(nonNilStringMap(object.Tags))
	result, err := s.exec(ctx, `UPDATE object_versions SET metadata_json = ?, tags_json = ?, retention_mode = ?, retain_until = ?, legal_hold = ? WHERE bucket = ? AND key = ? AND version_id = ?`, string(metadata), string(tags), object.RetentionMode, formatOptionalTime(object.RetainUntil), boolToInt(object.LegalHold), object.Bucket, object.Key, object.VersionID)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

const objectVersionSelect = `SELECT bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, metadata_json, tags_json, version_id, retention_mode, retain_until, legal_hold, is_delete_marker, created_at FROM object_versions`

func scanObjectVersion(row scanner) (domain.ObjectRecord, error) {
	var object domain.ObjectRecord
	var metadata, tags, retainUntil, created string
	var legalHold, deleteMarker int
	err := row.Scan(&object.Bucket, &object.Key, &object.ProviderAccountID, &object.RemoteBucket, &object.RemoteKey, &object.Size, &object.ContentType, &object.ETag, &object.ChecksumSHA256, &metadata, &tags, &object.VersionID, &object.RetentionMode, &retainUntil, &legalHold, &deleteMarker, &created)
	if err != nil {
		return object, err
	}
	_ = json.Unmarshal([]byte(metadata), &object.Metadata)
	_ = json.Unmarshal([]byte(tags), &object.Tags)
	object.RetainUntil = parseOptionalTime(retainUntil)
	object.LegalHold = legalHold == 1
	object.IsDeleteMarker = deleteMarker == 1
	object.CreatedAt = parseOptionalTime(created)
	object.UpdatedAt = object.CreatedAt
	object.ReplicaStatus = "none"
	return object, nil
}

func (s *Store) PutTrashObject(ctx context.Context, trash domain.TrashRecord) error {
	metadata, _ := json.Marshal(nonNilStringMap(trash.Object.Metadata))
	tags, _ := json.Marshal(nonNilStringMap(trash.Object.Tags))
	_, err := s.exec(ctx, `INSERT INTO trash_objects (id, bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, metadata_json, tags_json, deleted_at, purge_after) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, trash.ID, trash.Object.Bucket, trash.Object.Key, trash.Object.ProviderAccountID, trash.Object.RemoteBucket, trash.Object.RemoteKey, trash.Object.Size, trash.Object.ContentType, trash.Object.ETag, trash.Object.ChecksumSHA256, string(metadata), string(tags), trash.DeletedAt.UTC().Format(time.RFC3339Nano), trash.PurgeAfter.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetTrashObject(ctx context.Context, id string) (domain.TrashRecord, error) {
	trash, err := scanTrashObject(s.queryRow(ctx, trashObjectSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TrashRecord{}, ErrNotFound
	}
	return trash, err
}

func (s *Store) ListTrashObjects(ctx context.Context, limit int) ([]domain.TrashRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.query(ctx, trashObjectSelect+` ORDER BY deleted_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.TrashRecord
	for rows.Next() {
		trash, err := scanTrashObject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, trash)
	}
	return result, rows.Err()
}

func (s *Store) ListTrashObjectsDue(ctx context.Context, now time.Time, limit int) ([]domain.TrashRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.query(ctx, trashObjectSelect+` WHERE purge_after <= ? ORDER BY purge_after ASC LIMIT ?`, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.TrashRecord
	for rows.Next() {
		trash, err := scanTrashObject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, trash)
	}
	return result, rows.Err()
}

func (s *Store) ListTrashObjectsForLifecycle(ctx context.Context, bucket, prefix string, deletedBefore time.Time, limit int) ([]domain.TrashRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.query(ctx, trashObjectSelect+` WHERE bucket = ? AND substr(key, 1, length(?)) = ? AND deleted_at <= ? ORDER BY deleted_at ASC LIMIT ?`, bucket, prefix, prefix, deletedBefore.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.TrashRecord
	for rows.Next() {
		trash, err := scanTrashObject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, trash)
	}
	return result, rows.Err()
}

func (s *Store) DeleteTrashObject(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM trash_objects WHERE id = ?`, id)
	return err
}

const trashObjectSelect = `SELECT id, bucket, key, provider_account_id, remote_bucket, remote_key, size, content_type, etag, checksum_sha256, metadata_json, tags_json, deleted_at, purge_after FROM trash_objects`

func scanTrashObject(row scanner) (domain.TrashRecord, error) {
	var trash domain.TrashRecord
	var metadata, tags, deleted, purge string
	err := row.Scan(&trash.ID, &trash.Object.Bucket, &trash.Object.Key, &trash.Object.ProviderAccountID, &trash.Object.RemoteBucket, &trash.Object.RemoteKey, &trash.Object.Size, &trash.Object.ContentType, &trash.Object.ETag, &trash.Object.ChecksumSHA256, &metadata, &tags, &deleted, &purge)
	if err != nil {
		return trash, err
	}
	_ = json.Unmarshal([]byte(metadata), &trash.Object.Metadata)
	_ = json.Unmarshal([]byte(tags), &trash.Object.Tags)
	trash.DeletedAt = parseOptionalTime(deleted)
	trash.PurgeAfter = parseOptionalTime(purge)
	trash.Object.CreatedAt = trash.DeletedAt
	trash.Object.UpdatedAt = trash.DeletedAt
	trash.Object.ReplicaStatus = "none"
	return trash, nil
}
