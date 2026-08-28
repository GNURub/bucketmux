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

func (s *Store) UpsertWASMPlugin(ctx context.Context, plugin domain.WASMPlugin) error {
	now := time.Now().UTC()
	if plugin.CreatedAt.IsZero() {
		plugin.CreatedAt = now
	}
	plugin.UpdatedAt = now
	events, err := json.Marshal(plugin.Events)
	if err != nil {
		return fmt.Errorf("encode plugin events: %w", err)
	}
	contentTypes, err := json.Marshal(plugin.ContentTypes)
	if err != nil {
		return fmt.Errorf("encode plugin content types: %w", err)
	}
	pluginConfig, err := json.Marshal(plugin.Config)
	if err != nil {
		return fmt.Errorf("encode plugin config: %w", err)
	}
	_, err = s.exec(ctx, `
INSERT INTO wasm_plugins (id, name, description, abi_version, module_base64, module_sha256, events_json, bucket_pattern, key_prefix, key_suffix, content_types_json, config_json, enabled, timeout_millis, memory_limit_bytes, max_input_bytes, max_output_bytes, max_attempts, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name,
  description=excluded.description,
  abi_version=excluded.abi_version,
  module_base64=excluded.module_base64,
  module_sha256=excluded.module_sha256,
  events_json=excluded.events_json,
  bucket_pattern=excluded.bucket_pattern,
  key_prefix=excluded.key_prefix,
  key_suffix=excluded.key_suffix,
  content_types_json=excluded.content_types_json,
  config_json=excluded.config_json,
  enabled=excluded.enabled,
  timeout_millis=excluded.timeout_millis,
  memory_limit_bytes=excluded.memory_limit_bytes,
  max_input_bytes=excluded.max_input_bytes,
  max_output_bytes=excluded.max_output_bytes,
  max_attempts=excluded.max_attempts,
  updated_at=excluded.updated_at
`, plugin.ID, plugin.Name, plugin.Description, plugin.ABIVersion, plugin.ModuleBase64, plugin.ModuleSHA256, string(events), plugin.BucketPattern, plugin.KeyPrefix, plugin.KeySuffix, string(contentTypes), string(pluginConfig), boolToInt(plugin.Enabled), plugin.TimeoutMillis, plugin.MemoryLimitBytes, plugin.MaxInputBytes, plugin.MaxOutputBytes, plugin.MaxAttempts, plugin.CreatedAt.Format(time.RFC3339Nano), plugin.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert wasm plugin: %w", err)
	}
	return nil
}

func (s *Store) GetWASMPlugin(ctx context.Context, id string) (domain.WASMPlugin, error) {
	plugin, err := scanWASMPlugin(s.queryRow(ctx, wasmPluginSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WASMPlugin{}, ErrNotFound
	}
	return plugin, err
}

func (s *Store) ListWASMPlugins(ctx context.Context, enabledOnly bool) ([]domain.WASMPlugin, error) {
	query := wasmPluginSelect
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY name ASC, id ASC`
	rows, err := s.query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var plugins []domain.WASMPlugin
	for rows.Next() {
		plugin, err := scanWASMPlugin(rows)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, rows.Err()
}

func (s *Store) DeleteWASMPlugin(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM object_embeddings WHERE plugin_id = ?`), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM wasm_plugins WHERE id = ?`), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateWASMPluginJob(ctx context.Context, job domain.WASMPluginJob) (bool, error) {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.NextAttemptAt.IsZero() {
		job.NextAttemptAt = now
	}
	if job.Status == "" {
		job.Status = domain.WASMPluginStatusPending
	}
	result, err := s.exec(ctx, `INSERT INTO wasm_plugin_jobs (id, plugin_id, event, bucket, key, source_checksum, source_updated_at, dedupe_key, status, attempts, max_attempts, next_attempt_at, last_error, result_json, finished_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(dedupe_key) DO NOTHING`, job.ID, job.PluginID, job.Event, job.Bucket, job.Key, job.SourceChecksum, formatOptionalTime(job.SourceUpdatedAt), job.DedupeKey, job.Status, job.Attempts, job.MaxAttempts, job.NextAttemptAt.Format(time.RFC3339Nano), job.LastError, job.ResultJSON, formatOptionalTime(job.FinishedAt), job.CreatedAt.Format(time.RFC3339Nano), job.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Store) GetWASMPluginJob(ctx context.Context, id string) (domain.WASMPluginJob, error) {
	job, err := scanWASMPluginJob(s.queryRow(ctx, wasmPluginJobSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WASMPluginJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ListWASMPluginJobs(ctx context.Context, limit int) ([]domain.WASMPluginJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.query(ctx, wasmPluginJobSelect+` ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []domain.WASMPluginJob
	for rows.Next() {
		job, err := scanWASMPluginJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ClaimNextWASMPluginJob(ctx context.Context, now time.Time) (domain.WASMPluginJob, bool, error) {
	rows, err := s.query(ctx, `SELECT id FROM wasm_plugin_jobs WHERE status = ? AND next_attempt_at <= ? ORDER BY next_attempt_at ASC LIMIT 5`, domain.WASMPluginStatusPending, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return domain.WASMPluginJob{}, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return domain.WASMPluginJob{}, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return domain.WASMPluginJob{}, false, err
	}
	for _, id := range ids {
		result, err := s.exec(ctx, `UPDATE wasm_plugin_jobs SET status = ?, attempts = attempts + 1, updated_at = ? WHERE id = ? AND status = ?`, domain.WASMPluginStatusRunning, now.UTC().Format(time.RFC3339Nano), id, domain.WASMPluginStatusPending)
		if err != nil {
			return domain.WASMPluginJob{}, false, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			job, err := s.GetWASMPluginJob(ctx, id)
			return job, true, err
		}
	}
	return domain.WASMPluginJob{}, false, nil
}

func (s *Store) UpdateWASMPluginJob(ctx context.Context, job domain.WASMPluginJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.exec(ctx, `UPDATE wasm_plugin_jobs SET status = ?, attempts = ?, max_attempts = ?, next_attempt_at = ?, last_error = ?, result_json = ?, finished_at = ?, updated_at = ? WHERE id = ?`, job.Status, job.Attempts, job.MaxAttempts, job.NextAttemptAt.Format(time.RFC3339Nano), job.LastError, job.ResultJSON, formatOptionalTime(job.FinishedAt), job.UpdatedAt.Format(time.RFC3339Nano), job.ID)
	return err
}

func (s *Store) TouchWASMPluginJob(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `UPDATE wasm_plugin_jobs SET updated_at = ? WHERE id = ? AND status = ?`, time.Now().UTC().Format(time.RFC3339Nano), id, domain.WASMPluginStatusRunning)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecoverStaleWASMPluginJobs(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.exec(ctx, `UPDATE wasm_plugin_jobs SET status = ?, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE status = ? AND updated_at < ?`, domain.WASMPluginStatusPending, "recovered after worker interruption", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), domain.WASMPluginStatusRunning, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const wasmPluginSelect = `SELECT id, name, description, abi_version, module_base64, module_sha256, events_json, bucket_pattern, key_prefix, key_suffix, content_types_json, config_json, enabled, timeout_millis, memory_limit_bytes, max_input_bytes, max_output_bytes, max_attempts, created_at, updated_at FROM wasm_plugins`

func scanWASMPlugin(row scanner) (domain.WASMPlugin, error) {
	var plugin domain.WASMPlugin
	var eventsJSON, contentTypesJSON, configJSON, created, updated string
	var enabled int
	if err := row.Scan(&plugin.ID, &plugin.Name, &plugin.Description, &plugin.ABIVersion, &plugin.ModuleBase64, &plugin.ModuleSHA256, &eventsJSON, &plugin.BucketPattern, &plugin.KeyPrefix, &plugin.KeySuffix, &contentTypesJSON, &configJSON, &enabled, &plugin.TimeoutMillis, &plugin.MemoryLimitBytes, &plugin.MaxInputBytes, &plugin.MaxOutputBytes, &plugin.MaxAttempts, &created, &updated); err != nil {
		return plugin, err
	}
	if err := json.Unmarshal([]byte(eventsJSON), &plugin.Events); err != nil {
		return plugin, err
	}
	if err := json.Unmarshal([]byte(contentTypesJSON), &plugin.ContentTypes); err != nil {
		return plugin, err
	}
	if err := json.Unmarshal([]byte(configJSON), &plugin.Config); err != nil {
		return plugin, err
	}
	plugin.Enabled = enabled != 0
	plugin.CreatedAt = parseOptionalTime(created)
	plugin.UpdatedAt = parseOptionalTime(updated)
	return plugin, nil
}

const wasmPluginJobSelect = `SELECT id, plugin_id, event, bucket, key, source_checksum, source_updated_at, dedupe_key, status, attempts, max_attempts, next_attempt_at, last_error, result_json, finished_at, created_at, updated_at FROM wasm_plugin_jobs`

func scanWASMPluginJob(row scanner) (domain.WASMPluginJob, error) {
	var job domain.WASMPluginJob
	var sourceUpdated, nextAttempt, finished, created, updated string
	if err := row.Scan(&job.ID, &job.PluginID, &job.Event, &job.Bucket, &job.Key, &job.SourceChecksum, &sourceUpdated, &job.DedupeKey, &job.Status, &job.Attempts, &job.MaxAttempts, &nextAttempt, &job.LastError, &job.ResultJSON, &finished, &created, &updated); err != nil {
		return job, err
	}
	job.NextAttemptAt = parseOptionalTime(nextAttempt)
	job.SourceUpdatedAt = parseOptionalTime(sourceUpdated)
	job.FinishedAt = parseOptionalTime(finished)
	job.CreatedAt = parseOptionalTime(created)
	job.UpdatedAt = parseOptionalTime(updated)
	return job, nil
}
