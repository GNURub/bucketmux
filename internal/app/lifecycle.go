package app

import (
	"context"
	"strings"
	"time"
)

const lifecycleWorkerInterval = 10 * time.Minute

type LifecycleRunResult struct {
	ExpiredObjects int `json:"expired_objects"`
	PurgedTrash    int `json:"purged_trash"`
	Failures       int `json:"failures"`
}

func (s *Service) StartLifecycleWorker(ctx context.Context) {
	run := func() {
		acquired, err := s.Store.AcquireMaintenanceLease(ctx, "lifecycle", time.Now().UTC(), 30*time.Minute)
		if err != nil {
			s.recordWorkerFailure("lifecycle", err)
			return
		}
		if !acquired {
			return
		}
		defer func() { _ = s.Store.ReleaseMaintenanceLease(ctx, "lifecycle") }()
		if _, err := s.RunLifecycleOnce(ctx, time.Now().UTC()); err != nil {
			s.recordWorkerFailure("lifecycle", err)
			return
		}
		s.recordWorkerSuccess("lifecycle")
	}
	run()
	ticker := time.NewTicker(lifecycleWorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Service) RunLifecycleOnce(ctx context.Context, now time.Time) (LifecycleRunResult, error) {
	var result LifecycleRunResult
	buckets, err := s.Store.ListBuckets(ctx)
	if err != nil {
		return result, err
	}
	for _, bucket := range buckets {
		for _, rule := range bucket.LifecycleRules {
			if !rule.Enabled {
				continue
			}
			if rule.ExpireAfterDays > 0 {
				startAfter := ""
				for {
					objects, err := s.Store.ListObjectsAfter(ctx, bucket.Name, rule.Prefix, startAfter, 1000)
					if err != nil {
						return result, err
					}
					for _, object := range objects {
						if !strings.HasPrefix(object.Key, rule.Prefix) || object.UpdatedAt.After(now.Add(-time.Duration(rule.ExpireAfterDays)*24*time.Hour)) {
							continue
						}
						if _, err := s.DeleteObjectWithOptions(ctx, bucket.Name, object.Key, DeleteObjectOptions{}); err != nil {
							result.Failures++
						} else {
							result.ExpiredObjects++
						}
					}
					if len(objects) < 1000 {
						break
					}
					startAfter = objects[len(objects)-1].Key
				}
			}
			if rule.PurgeTrashAfterDays > 0 {
				for {
					trashObjects, err := s.Store.ListTrashObjectsForLifecycle(ctx, bucket.Name, rule.Prefix, now.Add(-time.Duration(rule.PurgeTrashAfterDays)*24*time.Hour), 100)
					if err != nil {
						return result, err
					}
					for _, trash := range trashObjects {
						if err := s.PurgeTrashObject(ctx, trash.ID); err != nil {
							result.Failures++
						} else {
							result.PurgedTrash++
						}
					}
					if len(trashObjects) < 100 {
						break
					}
				}
			}
		}
	}
	for {
		due, err := s.Store.ListTrashObjectsDue(ctx, now, 100)
		if err != nil {
			return result, err
		}
		for _, trash := range due {
			if err := s.PurgeTrashObject(ctx, trash.ID); err != nil {
				result.Failures++
			} else {
				result.PurgedTrash++
			}
		}
		if len(due) < 100 {
			break
		}
	}
	return result, nil
}
