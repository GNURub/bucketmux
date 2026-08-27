package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceLeaseIsExclusiveAndRecoverable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	acquired, err := store.AcquireMaintenanceLease(t.Context(), "lifecycle", now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire=%v err=%v", acquired, err)
	}
	acquired, err = store.AcquireMaintenanceLease(t.Context(), "lifecycle", now.Add(time.Second), time.Minute)
	if err != nil || acquired {
		t.Fatalf("second acquire=%v err=%v", acquired, err)
	}
	acquired, err = store.AcquireMaintenanceLease(t.Context(), "lifecycle", now.Add(2*time.Minute), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("stale acquire=%v err=%v", acquired, err)
	}
}
