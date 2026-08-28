package store

import (
	"context"
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

// ReserveProviderCapacity atomically reserves storage and monthly upload
// quota. The database update is the authority for both single- and
// multi-instance placement; router snapshots are only an ordering hint.
func (s *Store) ReserveProviderCapacity(ctx context.Context, reservation domain.ProviderReservation, marginBytes, monthlyLimitBytes int64, period string) (bool, error) {
	if reservation.ID == "" || reservation.ProviderAccountID == "" || reservation.Bytes < 0 {
		return false, fmt.Errorf("reservation id, provider id and non-negative bytes are required")
	}
	if reservation.CreatedAt.IsZero() {
		reservation.CreatedAt = time.Now().UTC()
	}
	if reservation.ExpiresAt.IsZero() {
		reservation.ExpiresAt = reservation.CreatedAt.Add(time.Hour)
	}
	if marginBytes < 0 {
		marginBytes = 0
	}
	if monthlyLimitBytes < 0 {
		monthlyLimitBytes = 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE provider_accounts SET monthly_uploaded_bytes = 0, monthly_period = ? WHERE id = ? AND monthly_period <> ?`), period, reservation.ProviderAccountID, period); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, s.rebind(`
UPDATE provider_accounts SET reserved_bytes = reserved_bytes + ?, monthly_period = ?, updated_at = ?
WHERE id = ? AND enabled = 1
  AND (availability_status = '' OR (availability_status IN ('throttled', 'unavailable') AND unavailable_until <> '' AND unavailable_until <= ?))
  AND (capacity_bytes <= 0 OR used_bytes + reserved_bytes + ? <= CASE WHEN capacity_bytes > ? THEN capacity_bytes - ? ELSE 0 END)
  AND (remote_capacity_bytes <= 0 OR remote_used_bytes + reserved_bytes + ? <= CASE WHEN remote_capacity_bytes > ? THEN remote_capacity_bytes - ? ELSE 0 END)
  AND (? <= 0 OR monthly_uploaded_bytes + reserved_bytes + ? <= ?)
`), reservation.Bytes, period, now, reservation.ProviderAccountID, now,
		reservation.Bytes, marginBytes, marginBytes,
		reservation.Bytes, marginBytes, marginBytes,
		monthlyLimitBytes, reservation.Bytes, monthlyLimitBytes)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO provider_reservations (id, provider_account_id, bytes, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`), reservation.ID, reservation.ProviderAccountID, reservation.Bytes, reservation.CreatedAt.UTC().Format(time.RFC3339Nano), reservation.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CommitProviderReservation(ctx context.Context, reservationID string, actualBytes int64) error {
	if actualBytes < 0 {
		return fmt.Errorf("actual bytes cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Make the first transaction statement a write. This avoids read-to-write
	// transaction upgrades and remains safe for legacy files opened by Turso.
	result, err := tx.ExecContext(ctx, s.rebind(`
UPDATE provider_accounts SET
  reserved_bytes = CASE
    WHEN reserved_bytes < COALESCE((SELECT bytes FROM provider_reservations WHERE id = ?), 0) THEN 0
    ELSE reserved_bytes - COALESCE((SELECT bytes FROM provider_reservations WHERE id = ?), 0)
  END,
  used_bytes = used_bytes + ?,
  monthly_uploaded_bytes = monthly_uploaded_bytes + ?,
  availability_status = '', availability_message = '', unavailable_until = '', updated_at = ?
WHERE id = (SELECT provider_account_id FROM provider_reservations WHERE id = ?)
`), reservationID, reservationID, actualBytes, actualBytes, time.Now().UTC().Format(time.RFC3339Nano), reservationID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM provider_reservations WHERE id = ?`), reservationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReleaseProviderReservation(ctx context.Context, reservationID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, s.rebind(`
UPDATE provider_accounts SET
  reserved_bytes = CASE
    WHEN reserved_bytes < COALESCE((SELECT bytes FROM provider_reservations WHERE id = ?), 0) THEN 0
    ELSE reserved_bytes - COALESCE((SELECT bytes FROM provider_reservations WHERE id = ?), 0)
  END,
  updated_at = ?
WHERE id = (SELECT provider_account_id FROM provider_reservations WHERE id = ?)
`), reservationID, reservationID, time.Now().UTC().Format(time.RFC3339Nano), reservationID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM provider_reservations WHERE id = ?`), reservationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverExpiredProviderReservations(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// Concurrent instances can be at slightly different points in startup.
	// Take the same transaction-scoped advisory lock as schema migration before
	// touching both quota tables, so recovery cannot deadlock with ALTER TABLE.
	if s.dialect == dialectPostgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", postgresSchemaLockID); err != nil {
			return 0, fmt.Errorf("coordinate reservation recovery with schema migration: %w", err)
		}
	}
	cutoff := before.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, s.rebind(`
UPDATE provider_accounts SET
  reserved_bytes = CASE
    WHEN reserved_bytes < COALESCE((SELECT SUM(r.bytes) FROM provider_reservations r WHERE r.provider_account_id = provider_accounts.id AND r.expires_at <= ?), 0) THEN 0
    ELSE reserved_bytes - COALESCE((SELECT SUM(r.bytes) FROM provider_reservations r WHERE r.provider_account_id = provider_accounts.id AND r.expires_at <= ?), 0)
  END,
  updated_at = ?
WHERE EXISTS (SELECT 1 FROM provider_reservations r WHERE r.provider_account_id = provider_accounts.id AND r.expires_at <= ?)
`), cutoff, cutoff, time.Now().UTC().Format(time.RFC3339Nano), cutoff); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM provider_reservations WHERE expires_at <= ?`), cutoff)
	if err != nil {
		return 0, err
	}
	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}

func (s *Store) SetProviderUsage(ctx context.Context, providerID string, usedBytes int64, source string, checkedAt time.Time) error {
	if usedBytes < 0 {
		usedBytes = 0
	}
	_, err := s.exec(ctx, `UPDATE provider_accounts SET used_bytes = ?, quota_source = ?, quota_checked_at = ?, availability_status = CASE WHEN availability_status = 'quota' THEN '' ELSE availability_status END, availability_message = CASE WHEN availability_status = 'quota' THEN '' ELSE availability_message END, updated_at = ? WHERE id = ?`, usedBytes, source, checkedAt.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), providerID)
	return err
}

func (s *Store) SetProviderRemoteQuota(ctx context.Context, providerID string, capacityBytes, usedBytes int64, source string, checkedAt time.Time) error {
	if capacityBytes < 0 {
		capacityBytes = 0
	}
	if usedBytes < 0 {
		usedBytes = 0
	}
	_, err := s.exec(ctx, `UPDATE provider_accounts SET remote_capacity_bytes = ?, remote_used_bytes = ?, quota_source = ?, quota_checked_at = ?, updated_at = ? WHERE id = ?`, capacityBytes, usedBytes, source, checkedAt.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), providerID)
	return err
}

func (s *Store) MarkProviderUnavailable(ctx context.Context, providerID, status, message string, until time.Time) error {
	_, err := s.exec(ctx, `UPDATE provider_accounts SET availability_status = ?, availability_message = ?, unavailable_until = ?, updated_at = ? WHERE id = ?`, status, message, formatOptionalTime(until), time.Now().UTC().Format(time.RFC3339Nano), providerID)
	return err
}

func (s *Store) ClearProviderUnavailable(ctx context.Context, providerID string) error {
	_, err := s.exec(ctx, `UPDATE provider_accounts SET availability_status = '', availability_message = '', unavailable_until = '', updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), providerID)
	return err
}
