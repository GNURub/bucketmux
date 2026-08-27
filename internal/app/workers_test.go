package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/coordination"
)

func TestDurableWorkerReleasesClaimLeaseBeforeRunningTrackedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	releaseErr := errors.New("redis release failed")
	lease := &testWorkerLease{err: releaseErr}
	svc := &Service{Coordinator: testWorkerCoordinator{lease: lease}, WorkerLeaseTTL: time.Second, workerState: newWorkerRuntimeState()}
	var recovered atomic.Bool
	var claimed atomic.Bool
	runErr := errors.New("worker adapter failed")
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.runDurableWorker(ctx, durableWorker{
			name:       "test-worker",
			interval:   time.Hour,
			staleAfter: time.Minute,
			recover: func(context.Context, time.Time) error {
				recovered.Store(true)
				return nil
			},
			claim: func(context.Context) (durableWorkItem, bool, error) {
				if claimed.Swap(true) {
					return durableWorkItem{}, false, nil
				}
				return durableWorkItem{run: func(context.Context) error {
					if !lease.released.Load() {
						t.Error("claim lease was still held during work execution")
					}
					cancel()
					return runErr
				}}, true, nil
			},
		})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("durable worker did not stop after cancellation")
	}
	if !recovered.Load() {
		t.Fatal("stale work recovery did not run before claiming")
	}
	snapshots := svc.workerRuntimeSnapshots()
	var found bool
	for _, snapshot := range snapshots {
		if snapshot.Name == "test-worker" {
			found = true
			if snapshot.Failures != 2 || snapshot.LastError != runErr.Error() {
				t.Fatalf("worker snapshot = %+v", snapshot)
			}
		}
	}
	if !found {
		t.Fatal("worker failure was not observable")
	}
}

func TestDurableWorkerHeartbeatsTrackedWork(t *testing.T) {
	svc := &Service{workerState: newWorkerRuntimeState()}
	heartbeat := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	var releaseOnce sync.Once
	err := svc.runDurableWorkItem(context.Background(), durableWorker{name: "test-worker", heartbeatInterval: time.Millisecond}, durableWorkItem{
		run: func(context.Context) error {
			<-releaseRun
			return nil
		},
		heartbeat: func(context.Context) error {
			select {
			case heartbeat <- struct{}{}:
			default:
			}
			releaseOnce.Do(func() { close(releaseRun) })
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runDurableWorkItem() error = %v", err)
	}
	select {
	case <-heartbeat:
	default:
		t.Fatal("durable work was not heartbeated")
	}
}

type testWorkerCoordinator struct {
	lease coordination.Lease
}

func (c testWorkerCoordinator) TryAcquire(context.Context, string, time.Duration) (coordination.Lease, bool, error) {
	return c.lease, true, nil
}

func (testWorkerCoordinator) Ping(context.Context) error { return nil }

type testWorkerLease struct {
	released atomic.Bool
	err      error
}

func (l *testWorkerLease) Release(context.Context) error {
	l.released.Store(true)
	return l.err
}
