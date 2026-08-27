package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) CreateRepairJob(ctx context.Context, job domain.RepairJob) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.Status == "" {
		job.Status = domain.RepairStatusPending
	}
	_, err := s.exec(ctx, `INSERT INTO repair_jobs (id, bucket, prefix, status, checked_objects, repaired_objects, failed_objects, current_key, last_error, finished_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, job.Bucket, job.Prefix, job.Status, job.CheckedObjects, job.RepairedObjects, job.FailedObjects, job.CurrentKey, job.LastError, formatOptionalTime(job.FinishedAt), job.CreatedAt.Format(time.RFC3339Nano), job.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ClaimNextRepairJob(ctx context.Context) (domain.RepairJob, bool, error) {
	rows, err := s.query(ctx, `SELECT id FROM repair_jobs WHERE status = ? ORDER BY created_at ASC LIMIT 5`, domain.RepairStatusPending)
	if err != nil {
		return domain.RepairJob{}, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return domain.RepairJob{}, false, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		result, err := s.exec(ctx, `UPDATE repair_jobs SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, domain.RepairStatusRunning, time.Now().UTC().Format(time.RFC3339Nano), id, domain.RepairStatusPending)
		if err != nil {
			return domain.RepairJob{}, false, err
		}
		changed, _ := result.RowsAffected()
		if changed == 1 {
			job, err := s.GetRepairJob(ctx, id)
			return job, true, err
		}
	}
	return domain.RepairJob{}, false, nil
}

func (s *Store) GetRepairJob(ctx context.Context, id string) (domain.RepairJob, error) {
	job, err := scanRepairJob(s.queryRow(ctx, repairJobSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepairJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ListRepairJobs(ctx context.Context, limit int) ([]domain.RepairJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.query(ctx, repairJobSelect+` ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []domain.RepairJob
	for rows.Next() {
		job, err := scanRepairJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) UpdateRepairJob(ctx context.Context, job domain.RepairJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.exec(ctx, `UPDATE repair_jobs SET status = ?, checked_objects = ?, repaired_objects = ?, failed_objects = ?, current_key = ?, last_error = ?, finished_at = ?, updated_at = ? WHERE id = ?`, job.Status, job.CheckedObjects, job.RepairedObjects, job.FailedObjects, job.CurrentKey, job.LastError, formatOptionalTime(job.FinishedAt), job.UpdatedAt.Format(time.RFC3339Nano), job.ID)
	return err
}

func (s *Store) TouchRepairJob(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `UPDATE repair_jobs SET updated_at = ? WHERE id = ? AND status = ?`, time.Now().UTC().Format(time.RFC3339Nano), id, domain.RepairStatusRunning)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecoverStaleRepairJobs(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.exec(ctx, `UPDATE repair_jobs SET status = ?, last_error = ?, updated_at = ? WHERE status = ? AND updated_at < ?`, domain.RepairStatusPending, "recovered after worker interruption", time.Now().UTC().Format(time.RFC3339Nano), domain.RepairStatusRunning, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const repairJobSelect = `SELECT id, bucket, prefix, status, checked_objects, repaired_objects, failed_objects, current_key, last_error, finished_at, created_at, updated_at FROM repair_jobs`

func scanRepairJob(row scanner) (domain.RepairJob, error) {
	var job domain.RepairJob
	var finished, created, updated string
	if err := row.Scan(&job.ID, &job.Bucket, &job.Prefix, &job.Status, &job.CheckedObjects, &job.RepairedObjects, &job.FailedObjects, &job.CurrentKey, &job.LastError, &finished, &created, &updated); err != nil {
		return job, err
	}
	job.FinishedAt = parseOptionalTime(finished)
	job.CreatedAt = parseOptionalTime(created)
	job.UpdatedAt = parseOptionalTime(updated)
	return job, nil
}
