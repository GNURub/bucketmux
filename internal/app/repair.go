package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const repairWorkerInterval = 2 * time.Second
const repairHeartbeatInterval = 30 * time.Second
const repairWorkStaleAfter = 15 * time.Minute

type RepairResult struct {
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	Repaired bool   `json:"repaired"`
	Message  string `json:"message"`
}

func (s *Service) RepairObject(ctx context.Context, bucket, key string) (RepairResult, error) {
	object, account, err := s.Store.GetObjectWithProvider(ctx, bucket, key)
	if err != nil {
		return RepairResult{}, err
	}
	decrypted, adapter, err := s.providerForReplica(ctx, account)
	if err == nil {
		if _, headErr := adapter.Head(ctx, decrypted, object); headErr == nil {
			return RepairResult{Bucket: bucket, Key: key, Message: "primary object is healthy"}, nil
		}
	}
	body, _, err := s.getObjectFromReplica(ctx, object)
	if err != nil {
		return RepairResult{}, fmt.Errorf("no readable replica is available: %w", err)
	}
	defer func() { _ = body.Close() }()
	stored, err := s.putOnProvider(ctx, account, domain.PutObjectInput{Bucket: object.Bucket, Key: object.Key, RemoteKey: object.RemoteKey, Size: object.Size, ContentType: object.ContentType, Metadata: object.Metadata, Tags: object.Tags}, body)
	if err != nil {
		return RepairResult{}, fmt.Errorf("restore primary object: %w", err)
	}
	object.RemoteBucket = stored.RemoteBucket
	object.RemoteKey = stored.RemoteKey
	object.ETag = stored.ETag
	object.Size = stored.Size
	if err := s.Store.PutObject(ctx, object); err != nil {
		return RepairResult{}, err
	}
	return RepairResult{Bucket: bucket, Key: key, Repaired: true, Message: "primary object restored from a replica"}, nil
}

func (s *Service) CreateRepairJob(ctx context.Context, bucket, prefix string) (domain.RepairJob, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.TrimLeft(strings.TrimSpace(prefix), "/")
	if bucket == "" {
		return domain.RepairJob{}, fmt.Errorf("bucket is required")
	}
	if _, err := s.ensureBucket(ctx, bucket); err != nil {
		return domain.RepairJob{}, err
	}
	job := domain.RepairJob{ID: newRepairID(), Bucket: bucket, Prefix: prefix, Status: domain.RepairStatusPending}
	if err := s.Store.CreateRepairJob(ctx, job); err != nil {
		return domain.RepairJob{}, err
	}
	signalWorker(s.repairWake)
	return s.Store.GetRepairJob(ctx, job.ID)
}

func (s *Service) StartRepairWorker(ctx context.Context) {
	s.runDurableWorker(ctx, durableWorker{
		name:              "repair",
		interval:          repairWorkerInterval,
		heartbeatInterval: repairHeartbeatInterval,
		staleAfter:        repairWorkStaleAfter,
		wake:              s.repairWake,
		recover: func(ctx context.Context, cutoff time.Time) error {
			_, err := s.Store.RecoverStaleRepairJobs(ctx, cutoff)
			return err
		},
		claim: func(ctx context.Context) (durableWorkItem, bool, error) {
			job, claimed, err := s.Store.ClaimNextRepairJob(ctx)
			if err != nil || !claimed {
				return durableWorkItem{}, claimed, err
			}
			return durableWorkItem{
				run:       func(ctx context.Context) error { return s.RunRepairJob(ctx, job.ID) },
				heartbeat: func(ctx context.Context) error { return s.Store.TouchRepairJob(ctx, job.ID) },
			}, true, nil
		},
	})
}

func (s *Service) RunRepairJob(ctx context.Context, id string) error {
	job, err := s.Store.GetRepairJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == domain.RepairStatusCompleted || job.Status == domain.RepairStatusFailed {
		return nil
	}
	job.Status = domain.RepairStatusRunning
	if err := s.Store.UpdateRepairJob(ctx, job); err != nil {
		return err
	}
	startAfter := ""
	for {
		objects, err := s.Store.ListObjectsAfter(ctx, job.Bucket, job.Prefix, startAfter, 500)
		if err != nil {
			return s.failRepairJob(ctx, &job, err)
		}
		for _, object := range objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			job.CurrentKey = object.Key
			job.CheckedObjects++
			result, repairErr := s.RepairObject(ctx, object.Bucket, object.Key)
			if repairErr != nil {
				job.FailedObjects++
				job.LastError = fmt.Sprintf("%s: %v", object.Key, repairErr)
			} else if result.Repaired {
				job.RepairedObjects++
			}
		}
		if err := s.Store.UpdateRepairJob(ctx, job); err != nil {
			return s.failRepairJob(ctx, &job, err)
		}
		if len(objects) < 500 {
			break
		}
		startAfter = objects[len(objects)-1].Key
	}
	job.Status = domain.RepairStatusCompleted
	job.CurrentKey = ""
	job.FinishedAt = time.Now().UTC()
	return s.Store.UpdateRepairJob(ctx, job)
}

func (s *Service) failRepairJob(ctx context.Context, job *domain.RepairJob, cause error) error {
	job.Status = domain.RepairStatusFailed
	job.LastError = cause.Error()
	job.FinishedAt = time.Now().UTC()
	_ = s.Store.UpdateRepairJob(ctx, *job)
	return cause
}

func newRepairID() string {
	buffer := make([]byte, 8)
	_, _ = rand.Read(buffer)
	return "repair-" + hex.EncodeToString(buffer)
}
