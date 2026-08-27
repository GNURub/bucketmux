package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type durableWorkItem struct {
	run       func(context.Context) error
	heartbeat func(context.Context) error
}

type durableWorker struct {
	name              string
	interval          time.Duration
	heartbeatInterval time.Duration
	staleAfter        time.Duration
	wake              <-chan struct{}
	recover           func(context.Context, time.Time) error
	claim             func(context.Context) (durableWorkItem, bool, error)
}

type workerRuntimeState struct {
	mu          sync.RWMutex
	failures    map[string]uint64
	lastError   map[string]string
	lastSuccess map[string]time.Time
}

type workerRuntimeSnapshot struct {
	Name        string
	Failures    uint64
	LastError   string
	LastSuccess time.Time
}

func newWorkerRuntimeState() workerRuntimeState {
	return workerRuntimeState{
		failures:    map[string]uint64{},
		lastError:   map[string]string{},
		lastSuccess: map[string]time.Time{},
	}
}

func signalWorker(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (s *Service) runDurableWorker(ctx context.Context, worker durableWorker) {
	if worker.interval <= 0 {
		worker.interval = time.Second
	}
	process := func() {
		if worker.recover != nil && worker.staleAfter > 0 {
			if err := worker.recover(ctx, time.Now().UTC().Add(-worker.staleAfter)); err != nil {
				s.recordWorkerFailure(worker.name, err)
				return
			}
		}
		for ctx.Err() == nil {
			item, claimed, err := s.claimDurableWork(ctx, worker.name, worker.claim)
			if err != nil {
				s.recordWorkerFailure(worker.name, err)
				return
			}
			if !claimed {
				return
			}
			if err := s.runDurableWorkItem(ctx, worker, item); err != nil {
				s.recordWorkerFailure(worker.name, err)
				continue
			}
			s.recordWorkerSuccess(worker.name)
		}
	}

	process()
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			process()
		case <-worker.wake:
			process()
		}
	}
}

func (s *Service) runDurableWorkItem(ctx context.Context, worker durableWorker, item durableWorkItem) error {
	if item.run == nil {
		return nil
	}
	if item.heartbeat == nil || worker.heartbeatInterval <= 0 {
		return item.run(ctx)
	}

	done := make(chan error, 1)
	go func() {
		done <- item.run(ctx)
	}()
	ticker := time.NewTicker(worker.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if err := item.heartbeat(ctx); err != nil {
				s.recordWorkerFailure(worker.name, fmt.Errorf("heartbeat: %w", err))
			}
		case <-ctx.Done():
			// The work function receives the same context. Waiting here keeps the
			// goroutine inside Service.workerWG even if an adapter is slow to stop.
			return <-done
		}
	}
}

func (s *Service) claimDurableWork(ctx context.Context, workerName string, claim func(context.Context) (durableWorkItem, bool, error)) (item durableWorkItem, claimed bool, err error) {
	if claim == nil {
		return durableWorkItem{}, false, nil
	}
	coordinator := s.Coordinator
	if coordinator == nil {
		return claim(ctx)
	}
	ttl := s.WorkerLeaseTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	lease, acquired, err := coordinator.TryAcquire(ctx, workerName+":claim", ttl)
	if err != nil || !acquired {
		return durableWorkItem{}, false, err
	}
	defer func() {
		releaseErr := lease.Release(ctx)
		if releaseErr == nil {
			return
		}
		if err != nil {
			err = errors.Join(err, releaseErr)
		} else if claimed {
			// The database claim is already the durable ownership boundary. A
			// failed lease release must not strand claimed work until recovery.
			s.recordWorkerFailure(workerName, fmt.Errorf("release claim lease: %w", releaseErr))
		} else {
			err = releaseErr
		}
	}()
	return claim(ctx)
}

func (s *Service) recordWorkerFailure(name string, err error) {
	if err == nil {
		return
	}
	s.workerState.mu.Lock()
	defer s.workerState.mu.Unlock()
	s.workerState.failures[name]++
	s.workerState.lastError[name] = err.Error()
}

func (s *Service) recordWorkerSuccess(name string) {
	s.workerState.mu.Lock()
	defer s.workerState.mu.Unlock()
	s.workerState.lastSuccess[name] = time.Now().UTC()
	s.workerState.lastError[name] = ""
}

func (s *Service) workerRuntimeSnapshots() []workerRuntimeSnapshot {
	s.workerState.mu.RLock()
	defer s.workerState.mu.RUnlock()
	names := []string{"hooks", "inventory", "lifecycle", "migration", "repair", "replication"}
	seen := map[string]bool{"hooks": true, "inventory": true, "lifecycle": true, "migration": true, "repair": true, "replication": true}
	for name := range s.workerState.failures {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	for name := range s.workerState.lastSuccess {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]workerRuntimeSnapshot, 0, len(names))
	for _, name := range names {
		out = append(out, workerRuntimeSnapshot{Name: name, Failures: s.workerState.failures[name], LastError: s.workerState.lastError[name], LastSuccess: s.workerState.lastSuccess[name]})
	}
	return out
}
