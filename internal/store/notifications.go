package store

import (
	"context"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Store) ReplaceBucketNotifications(ctx context.Context, bucket string, notifications []domain.BucketNotification) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM bucket_notifications WHERE bucket = ?`), bucket); err != nil {
		return err
	}
	for _, notification := range notifications {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO bucket_notifications (id, bucket, hook_id, event, prefix, suffix) VALUES (?, ?, ?, ?, ?, ?)`), notification.ID, bucket, notification.HookID, notification.Event, notification.Prefix, notification.Suffix); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListBucketNotifications(ctx context.Context, bucket string) ([]domain.BucketNotification, error) {
	rows, err := s.query(ctx, `SELECT id, bucket, hook_id, event, prefix, suffix FROM bucket_notifications WHERE bucket = ? ORDER BY id, event`, bucket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []domain.BucketNotification
	for rows.Next() {
		var notification domain.BucketNotification
		if err := rows.Scan(&notification.ID, &notification.Bucket, &notification.HookID, &notification.Event, &notification.Prefix, &notification.Suffix); err != nil {
			return nil, err
		}
		result = append(result, notification)
	}
	return result, rows.Err()
}
