package store

import (
	"context"
	"time"
)

func (s *Store) AcquireMaintenanceLease(ctx context.Context, name string, now time.Time, ttl time.Duration) (bool, error) {
	nowText := now.UTC().Format(time.RFC3339Nano)
	untilText := now.UTC().Add(ttl).Format(time.RFC3339Nano)
	result, err := s.exec(ctx, `INSERT INTO maintenance_leases (name, leased_until, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET leased_until=excluded.leased_until, updated_at=excluded.updated_at WHERE maintenance_leases.leased_until < ?`, name, untilText, nowText, nowText)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Store) ReleaseMaintenanceLease(ctx context.Context, name string) error {
	now := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	_, err := s.exec(ctx, `UPDATE maintenance_leases SET leased_until = ?, updated_at = ? WHERE name = ?`, now, now, name)
	return err
}
