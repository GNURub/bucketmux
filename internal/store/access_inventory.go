package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) UpsertAccessCredential(ctx context.Context, credential domain.AccessCredential) error {
	now := time.Now().UTC()
	if credential.CreatedAt.IsZero() {
		credential.CreatedAt = now
	}
	credential.UpdatedAt = now
	permissions, err := json.Marshal(credential.Permissions)
	if err != nil {
		return fmt.Errorf("encode credential permissions: %w", err)
	}
	buckets, err := json.Marshal(credential.BucketPatterns)
	if err != nil {
		return fmt.Errorf("encode credential bucket patterns: %w", err)
	}
	prefixes, err := json.Marshal(credential.PrefixPatterns)
	if err != nil {
		return fmt.Errorf("encode credential prefix patterns: %w", err)
	}
	_, err = s.exec(ctx, `
INSERT INTO access_credentials (id, name, access_key, secret_encrypted, role, permissions_json, bucket_patterns_json, prefix_patterns_json, enabled, expires_at, last_used_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, access_key=excluded.access_key, secret_encrypted=excluded.secret_encrypted, role=excluded.role, permissions_json=excluded.permissions_json, bucket_patterns_json=excluded.bucket_patterns_json, prefix_patterns_json=excluded.prefix_patterns_json, enabled=excluded.enabled, expires_at=excluded.expires_at, updated_at=excluded.updated_at
`, credential.ID, credential.Name, credential.AccessKey, credential.SecretEncrypted, credential.Role, string(permissions), string(buckets), string(prefixes), boolToInt(credential.Enabled), formatOptionalTime(credential.ExpiresAt), formatOptionalTime(credential.LastUsedAt), credential.CreatedAt.Format(time.RFC3339Nano), credential.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert access credential: %w", err)
	}
	return nil
}

func (s *Store) GetAccessCredentialByAccessKey(ctx context.Context, accessKey string) (domain.AccessCredential, error) {
	row := s.queryRow(ctx, accessCredentialSelect+` WHERE access_key = ?`, accessKey)
	credential, err := scanAccessCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AccessCredential{}, ErrNotFound
	}
	return credential, err
}

func (s *Store) GetAccessCredential(ctx context.Context, id string) (domain.AccessCredential, error) {
	credential, err := scanAccessCredential(s.queryRow(ctx, accessCredentialSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AccessCredential{}, ErrNotFound
	}
	return credential, err
}

func (s *Store) ListAccessCredentials(ctx context.Context) ([]domain.AccessCredential, error) {
	rows, err := s.query(ctx, accessCredentialSelect+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.AccessCredential
	for rows.Next() {
		credential, err := scanAccessCredential(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, credential)
	}
	return result, rows.Err()
}

func (s *Store) DeleteAccessCredential(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM access_credentials WHERE id = ?`, id)
	return err
}

func (s *Store) TouchAccessCredential(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.exec(ctx, `UPDATE access_credentials SET last_used_at = ?, updated_at = updated_at WHERE id = ?`, now, id)
	return err
}

const accessCredentialSelect = `SELECT id, name, access_key, secret_encrypted, role, permissions_json, bucket_patterns_json, prefix_patterns_json, enabled, expires_at, last_used_at, created_at, updated_at FROM access_credentials`

func scanAccessCredential(row scanner) (domain.AccessCredential, error) {
	var credential domain.AccessCredential
	var permissions, buckets, prefixes string
	var enabled int
	var expires, lastUsed, created, updated string
	if err := row.Scan(&credential.ID, &credential.Name, &credential.AccessKey, &credential.SecretEncrypted, &credential.Role, &permissions, &buckets, &prefixes, &enabled, &expires, &lastUsed, &created, &updated); err != nil {
		return credential, err
	}
	credential.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(permissions), &credential.Permissions)
	_ = json.Unmarshal([]byte(buckets), &credential.BucketPatterns)
	_ = json.Unmarshal([]byte(prefixes), &credential.PrefixPatterns)
	credential.ExpiresAt = parseOptionalTime(expires)
	credential.LastUsedAt = parseOptionalTime(lastUsed)
	credential.CreatedAt = parseOptionalTime(created)
	credential.UpdatedAt = parseOptionalTime(updated)
	return credential, nil
}

func (s *Store) CreateInventoryJob(ctx context.Context, job domain.InventoryJob) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = domain.InventoryStatusPending
	}
	_, err := s.exec(ctx, `INSERT INTO inventory_jobs (id, provider_account_id, bucket, remote_bucket, prefix, mode, status, discovered_objects, imported_objects, missing_objects, last_error, finished_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, job.ProviderAccountID, job.Bucket, job.RemoteBucket, job.Prefix, job.Mode, job.Status, job.DiscoveredObjects, job.ImportedObjects, job.MissingObjects, job.LastError, formatOptionalTime(job.FinishedAt), job.CreatedAt.Format(time.RFC3339Nano), job.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ClaimNextInventoryJob(ctx context.Context) (domain.InventoryJob, bool, error) {
	rows, err := s.query(ctx, `SELECT id FROM inventory_jobs WHERE status = ? ORDER BY created_at ASC LIMIT 5`, domain.InventoryStatusPending)
	if err != nil {
		return domain.InventoryJob{}, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return domain.InventoryJob{}, false, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := s.exec(ctx, `UPDATE inventory_jobs SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, domain.InventoryStatusRunning, now, id, domain.InventoryStatusPending)
		if err != nil {
			return domain.InventoryJob{}, false, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			job, err := s.GetInventoryJob(ctx, id)
			return job, true, err
		}
	}
	return domain.InventoryJob{}, false, nil
}

func (s *Store) GetInventoryJob(ctx context.Context, id string) (domain.InventoryJob, error) {
	job, err := scanInventoryJob(s.queryRow(ctx, inventoryJobSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InventoryJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ListInventoryJobs(ctx context.Context, limit int) ([]domain.InventoryJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.query(ctx, inventoryJobSelect+` ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []domain.InventoryJob
	for rows.Next() {
		job, err := scanInventoryJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) UpdateInventoryJob(ctx context.Context, job domain.InventoryJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.exec(ctx, `UPDATE inventory_jobs SET remote_bucket = ?, status = ?, discovered_objects = ?, imported_objects = ?, missing_objects = ?, last_error = ?, finished_at = ?, updated_at = ? WHERE id = ?`, job.RemoteBucket, job.Status, job.DiscoveredObjects, job.ImportedObjects, job.MissingObjects, job.LastError, formatOptionalTime(job.FinishedAt), job.UpdatedAt.Format(time.RFC3339Nano), job.ID)
	return err
}

func (s *Store) TouchInventoryJob(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `UPDATE inventory_jobs SET updated_at = ? WHERE id = ? AND status = ?`, time.Now().UTC().Format(time.RFC3339Nano), id, domain.InventoryStatusRunning)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecoverStaleInventoryJobs(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.exec(ctx, `UPDATE inventory_jobs SET status = ?, last_error = ?, updated_at = ? WHERE status = ? AND updated_at < ?`, domain.InventoryStatusPending, "recovered after worker interruption", time.Now().UTC().Format(time.RFC3339Nano), domain.InventoryStatusRunning, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const inventoryJobSelect = `SELECT id, provider_account_id, bucket, remote_bucket, prefix, mode, status, discovered_objects, imported_objects, missing_objects, last_error, finished_at, created_at, updated_at FROM inventory_jobs`

func scanInventoryJob(row scanner) (domain.InventoryJob, error) {
	var job domain.InventoryJob
	var finished, created, updated string
	if err := row.Scan(&job.ID, &job.ProviderAccountID, &job.Bucket, &job.RemoteBucket, &job.Prefix, &job.Mode, &job.Status, &job.DiscoveredObjects, &job.ImportedObjects, &job.MissingObjects, &job.LastError, &finished, &created, &updated); err != nil {
		return job, err
	}
	job.FinishedAt = parseOptionalTime(finished)
	job.CreatedAt = parseOptionalTime(created)
	job.UpdatedAt = parseOptionalTime(updated)
	return job, nil
}
