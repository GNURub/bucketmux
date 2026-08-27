package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) PutObjectAttributes(ctx context.Context, object domain.ObjectRecord) error {
	metadata, err := json.Marshal(nonNilStringMap(object.Metadata))
	if err != nil {
		return err
	}
	tags, err := json.Marshal(nonNilStringMap(object.Tags))
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO object_attributes (bucket, key, metadata_json, tags_json, version_id, retention_mode, retain_until, legal_hold, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(bucket, key) DO UPDATE SET metadata_json=excluded.metadata_json, tags_json=excluded.tags_json, version_id=excluded.version_id, retention_mode=excluded.retention_mode, retain_until=excluded.retain_until, legal_hold=excluded.legal_hold, updated_at=excluded.updated_at`, object.Bucket, object.Key, string(metadata), string(tags), object.VersionID, object.RetentionMode, formatOptionalTime(object.RetainUntil), boolToInt(object.LegalHold), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) HydrateObjectAttributes(ctx context.Context, object *domain.ObjectRecord) error {
	var metadata, tags, retainUntil string
	var legalHold int
	err := s.queryRow(ctx, `SELECT metadata_json, tags_json, version_id, retention_mode, retain_until, legal_hold FROM object_attributes WHERE bucket = ? AND key = ?`, object.Bucket, object.Key).Scan(&metadata, &tags, &object.VersionID, &object.RetentionMode, &retainUntil, &legalHold)
	if errors.Is(err, sql.ErrNoRows) {
		object.Metadata = map[string]string{}
		object.Tags = map[string]string{}
		return nil
	}
	if err != nil {
		return err
	}
	_ = json.Unmarshal([]byte(metadata), &object.Metadata)
	_ = json.Unmarshal([]byte(tags), &object.Tags)
	object.RetainUntil = parseOptionalTime(retainUntil)
	object.LegalHold = legalHold == 1
	return nil
}

func (s *Store) UpdateObjectTags(ctx context.Context, bucket, key string, tags map[string]string) error {
	object, err := s.GetObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	if err := s.HydrateObjectAttributes(ctx, &object); err != nil {
		return err
	}
	object.Tags = nonNilStringMap(tags)
	if err := s.PutObjectAttributes(ctx, object); err != nil {
		return err
	}
	if object.VersionID != "" {
		return s.UpdateObjectVersionProtection(ctx, object)
	}
	return nil
}

func nonNilStringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}
