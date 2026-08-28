package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestProviderCapacityReservationLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "quota", Name: "Quota", Kind: domain.ProviderKindLocal, Bucket: "images", CapacityBytes: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	first := domain.ProviderReservation{ID: "first", ProviderAccountID: "quota", Bytes: 60, ExpiresAt: time.Now().Add(time.Hour)}
	if ok, err := s.ReserveProviderCapacity(ctx, first, 10, 80, "2026-08"); err != nil || !ok {
		t.Fatalf("first reserve ok=%v err=%v", ok, err)
	}
	if ok, err := s.ReserveProviderCapacity(ctx, domain.ProviderReservation{ID: "too-large", ProviderAccountID: "quota", Bytes: 31}, 10, 80, "2026-08"); err != nil || ok {
		t.Fatalf("over capacity reserve ok=%v err=%v", ok, err)
	}
	account, _ := s.GetProvider(ctx, "quota")
	if account.ReservedBytes != 60 {
		t.Fatalf("reserved=%d want 60", account.ReservedBytes)
	}
	if err := s.ReleaseProviderReservation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	committed := domain.ProviderReservation{ID: "committed", ProviderAccountID: "quota", Bytes: 40}
	if ok, err := s.ReserveProviderCapacity(ctx, committed, 10, 80, "2026-08"); err != nil || !ok {
		t.Fatalf("commit reserve ok=%v err=%v", ok, err)
	}
	if err := s.CommitProviderReservation(ctx, committed.ID, 40); err != nil {
		t.Fatal(err)
	}
	account, _ = s.GetProvider(ctx, "quota")
	if account.UsedBytes != 40 || account.ReservedBytes != 0 || account.MonthlyUploadedBytes != 40 || account.MonthlyPeriod != "2026-08" {
		t.Fatalf("account after commit=%+v", account)
	}
	if ok, err := s.ReserveProviderCapacity(ctx, domain.ProviderReservation{ID: "monthly", ProviderAccountID: "quota", Bytes: 41}, 0, 80, "2026-08"); err != nil || ok {
		t.Fatalf("monthly limit reserve ok=%v err=%v", ok, err)
	}
	if ok, err := s.ReserveProviderCapacity(ctx, domain.ProviderReservation{ID: "new-month", ProviderAccountID: "quota", Bytes: 41}, 0, 80, "2026-09"); err != nil || !ok {
		t.Fatalf("new month reserve ok=%v err=%v", ok, err)
	}
}

func TestProviderCapacityReservationIsAtomicUnderConcurrency(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "atomic-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "shared", Name: "Shared", Kind: domain.ProviderKindLocal, Bucket: "images", CapacityBytes: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	for index := range 20 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ok, err := s.ReserveProviderCapacity(ctx, domain.ProviderReservation{ID: fmt.Sprintf("r-%d", index), ProviderAccountID: "shared", Bytes: 10}, 0, 0, "2026-08")
			if err != nil {
				failures.Add(1)
				return
			}
			if ok {
				successes.Add(1)
			}
		}(index)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("reservation errors=%d", failures.Load())
	}
	if successes.Load() != 10 {
		t.Fatalf("successful reservations=%d want 10", successes.Load())
	}
	account, _ := s.GetProvider(ctx, "shared")
	if account.ReservedBytes != 100 {
		t.Fatalf("reserved=%d want 100", account.ReservedBytes)
	}
}

func TestExpiredProviderReservationIsRecovered(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertProvider(ctx, domain.ProviderAccount{ID: "p", Name: "p", Kind: domain.ProviderKindLocal, Bucket: "images", CapacityBytes: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	reservation := domain.ProviderReservation{ID: "expired", ProviderAccountID: "p", Bytes: 10, ExpiresAt: time.Now().Add(-time.Minute)}
	if ok, err := s.ReserveProviderCapacity(ctx, reservation, 0, 0, "2026-08"); err != nil || !ok {
		t.Fatalf("reserve ok=%v err=%v", ok, err)
	}
	if count, err := s.RecoverExpiredProviderReservations(ctx, time.Now()); err != nil || count != 1 {
		t.Fatalf("recover count=%d err=%v", count, err)
	}
	account, _ := s.GetProvider(ctx, "p")
	if account.ReservedBytes != 0 {
		t.Fatalf("reserved=%d", account.ReservedBytes)
	}
}
