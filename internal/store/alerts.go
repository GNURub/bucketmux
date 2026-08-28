package store

import (
	"context"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) UpsertAlert(ctx context.Context, alert domain.Alert) error {
	now := time.Now().UTC()
	if alert.CreatedAt.IsZero() {
		alert.CreatedAt = now
	}
	alert.UpdatedAt = now
	if alert.Status == "" {
		alert.Status = domain.AlertStatusOpen
	}
	_, err := s.exec(ctx, `
INSERT INTO alerts (id, dedupe_key, type, severity, provider_account_id, bucket, key, message, status, resolved_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dedupe_key) DO UPDATE SET
  type=excluded.type,
  severity=excluded.severity,
  provider_account_id=excluded.provider_account_id,
  bucket=excluded.bucket,
  key=excluded.key,
  message=excluded.message,
  status='open',
  resolved_at='',
  updated_at=excluded.updated_at
`, alert.ID, alert.DedupeKey, alert.Type, alert.Severity, alert.ProviderAccountID, alert.Bucket, alert.Key, alert.Message, alert.Status, formatOptionalTime(alert.ResolvedAt), alert.CreatedAt.Format(time.RFC3339Nano), alert.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ResolveAlert(ctx context.Context, dedupeKey string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.exec(ctx, `UPDATE alerts SET status = ?, resolved_at = ?, updated_at = ? WHERE dedupe_key = ? AND status <> ?`, domain.AlertStatusResolved, now, now, dedupeKey, domain.AlertStatusResolved)
	return err
}

func (s *Store) ListAlerts(ctx context.Context, status string, limit int) ([]domain.Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, dedupe_key, type, severity, provider_account_id, bucket, key, message, status, resolved_at, created_at, updated_at FROM alerts`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var alerts []domain.Alert
	for rows.Next() {
		var alert domain.Alert
		var resolved, created, updated string
		if err := rows.Scan(&alert.ID, &alert.DedupeKey, &alert.Type, &alert.Severity, &alert.ProviderAccountID, &alert.Bucket, &alert.Key, &alert.Message, &alert.Status, &resolved, &created, &updated); err != nil {
			return nil, err
		}
		alert.ResolvedAt = parseOptionalTime(resolved)
		alert.CreatedAt = parseOptionalTime(created)
		alert.UpdatedAt = parseOptionalTime(updated)
		alerts = append(alerts, alert)
	}
	return alerts, rows.Err()
}
