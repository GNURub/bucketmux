package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/coordination"
	secretcrypto "github.com/gnurub/bucketmux/internal/crypto"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
	placement "github.com/gnurub/bucketmux/internal/router"
	"github.com/gnurub/bucketmux/internal/store"
)

type Service struct {
	Store     *store.Store
	Secrets   *secretcrypto.SecretBox
	Providers *provider.Registry
	Router    *placement.PlacementRouter
	Config    config.Config

	Coordinator    coordination.Coordinator
	WorkerLeaseTTL time.Duration
	HookHTTPClient *http.Client
	HookRetryDelay func(attempts int) time.Duration
	cancelWorkers  context.CancelFunc
	workerWG       sync.WaitGroup
}

func NewService(ctx context.Context, cfg config.Config) (*Service, error) {
	cfg.Normalize()
	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := store.OpenConfig(cfg.Store, cfg.Server.DBPath)
	if err != nil {
		return nil, err
	}
	secrets, err := secretcrypto.NewSecretBox(cfg.Server.MasterKey)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	coordinator := coordination.Coordinator(coordination.NoopCoordinator{})
	if cfg.Coordination.Kind == "redis" {
		coordinator = coordination.NewRedis(coordination.RedisConfig{
			Addr:      cfg.Coordination.Redis.Addr,
			Password:  cfg.Coordination.Redis.Password,
			DB:        cfg.Coordination.Redis.DB,
			KeyPrefix: cfg.Coordination.Redis.KeyPrefix,
		})
	}
	svc := &Service{
		Store:   db,
		Secrets: secrets,
		Providers: provider.NewRegistry(
			provider.Entry(domain.ProviderKindLocal, provider.NewLocalAdapter(cfg.Server.DataDir)),
			provider.Entry(domain.ProviderKindS3Compat, provider.NewS3CompatAdapter()),
			provider.Entry(domain.ProviderKindCloudinary, provider.NewCloudinaryAdapter()),
			provider.Entry(domain.ProviderKindVercelBlob, provider.NewVercelBlobAdapter()),
		),
		Router: placement.NewPlacementRouter(db),
		Config: cfg,

		Coordinator:    coordinator,
		WorkerLeaseTTL: time.Duration(cfg.Coordination.Redis.LeaseTTLSeconds) * time.Second,
		HookHTTPClient: &http.Client{Timeout: defaultHookTimeout},
		cancelWorkers:  cancelWorkers,
	}
	if err := svc.Bootstrap(ctx, cfg); err != nil {
		_ = db.Close()
		return nil, err
	}
	svc.workerWG.Add(1)
	go func() {
		defer svc.workerWG.Done()
		svc.StartHookDeliveryWorker(workerCtx)
	}()
	svc.workerWG.Add(1)
	go func() {
		defer svc.workerWG.Done()
		svc.StartMigrationWorker(workerCtx)
	}()
	svc.workerWG.Add(1)
	go func() {
		defer svc.workerWG.Done()
		svc.StartReplicationWorker(workerCtx)
	}()
	return svc, nil
}

func (s *Service) Close() error {
	if s.cancelWorkers != nil {
		s.cancelWorkers()
	}
	s.workerWG.Wait()
	return s.Store.Close()
}

func (s *Service) Bootstrap(ctx context.Context, cfg config.Config) error {
	for _, b := range cfg.Buckets {
		if b.Name == "" {
			continue
		}
		replicationProviderIDs := normalizeProviderIDs(b.ReplicationProviderIDs)
		if err := s.Store.UpsertBucket(ctx, domain.Bucket{Name: b.Name, ReplicationEnabled: len(replicationProviderIDs) > 0 || b.ReplicationEnabled, ReplicationProviderIDs: replicationProviderIDs}); err != nil {
			return fmt.Errorf("bootstrap bucket %s: %w", b.Name, err)
		}
	}
	for _, p := range cfg.Providers {
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		secretEncrypted := ""
		if p.SecretKey != "" {
			var err error
			secretEncrypted, err = s.Secrets.Encrypt(p.SecretKey)
			if err != nil {
				return fmt.Errorf("encrypt provider %s secret: %w", p.ID, err)
			}
		}
		if p.Settings == nil {
			p.Settings = map[string]string{}
		}
		account := domain.ProviderAccount{
			ID:              p.ID,
			Name:            p.Name,
			Kind:            domain.ProviderKind(p.Kind),
			Endpoint:        p.Endpoint,
			Region:          p.Region,
			Bucket:          p.Bucket,
			AccessKey:       p.AccessKey,
			SecretEncrypted: secretEncrypted,
			CapacityBytes:   p.CapacityBytes,
			Priority:        p.Priority,
			Enabled:         enabled,
			Settings:        p.Settings,
		}
		if err := s.Store.UpsertProvider(ctx, account); err != nil {
			return fmt.Errorf("bootstrap provider %s: %w", p.ID, err)
		}
	}
	return nil
}

func (s *Service) PutObject(ctx context.Context, input domain.PutObjectInput, body io.Reader) (domain.ObjectRecord, error) {
	bucket, err := s.ensureBucket(ctx, input.Bucket)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	primary, err := s.Router.Choose(ctx, input, nil)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	targets := replicaTargets(bucket, primary.ID)
	var source io.Reader = body
	var cleanup func()
	if len(targets) > 0 {
		source, cleanup, err = s.spoolUpload(body)
		if err != nil {
			return domain.ObjectRecord{}, err
		}
		defer cleanup()
	}
	stored, err := s.putOnProvider(ctx, primary, input, source)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	replicaStatus := "none"
	if len(targets) > 0 {
		replicaStatus = "pending"
	}
	obj := domain.ObjectRecord{
		Bucket:            input.Bucket,
		Key:               input.Key,
		ProviderAccountID: stored.ProviderAccountID,
		RemoteBucket:      stored.RemoteBucket,
		RemoteKey:         stored.RemoteKey,
		Size:              stored.Size,
		ContentType:       stored.ContentType,
		ETag:              stored.ETag,
		ChecksumSHA256:    stored.ChecksumSHA256,
		ReplicaStatus:     replicaStatus,
	}
	if err := s.Store.PutObject(ctx, obj); err != nil {
		return domain.ObjectRecord{}, err
	}
	_ = s.Store.AddProviderUsage(ctx, primary.ID, stored.Size)
	if len(targets) > 0 {
		_ = s.enqueueObjectReplicas(ctx, obj, targets)
	}
	s.dispatchObjectHook(ctx, domain.HookEventObjectCreated, obj)
	return obj, nil
}

func (s *Service) tryWorkerLease(ctx context.Context, name string) (coordination.Lease, bool) {
	if s.Coordinator == nil {
		lease, ok, _ := coordination.NoopCoordinator{}.TryAcquire(ctx, name, s.WorkerLeaseTTL)
		return lease, ok
	}
	ttl := s.WorkerLeaseTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	lease, ok, err := s.Coordinator.TryAcquire(ctx, name, ttl)
	if err != nil || !ok {
		return nil, false
	}
	return lease, true
}

func (s *Service) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, domain.ObjectRecord, error) {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, obj, err
	}
	account, adapter, err := s.providerForObject(ctx, obj)
	if err != nil {
		return nil, obj, err
	}
	return adapter.Get(ctx, account, obj)
}

func (s *Service) HeadObject(ctx context.Context, bucket, key string) (domain.ObjectRecord, error) {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		return obj, err
	}
	account, adapter, err := s.providerForObject(ctx, obj)
	if err != nil {
		return obj, err
	}
	return adapter.Head(ctx, account, obj)
}

func (s *Service) DeleteObject(ctx context.Context, bucket, key string) error {
	obj, err := s.Store.GetObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	account, adapter, err := s.providerForObject(ctx, obj)
	if err != nil {
		return err
	}
	if err := adapter.Delete(ctx, account, obj); err != nil {
		return err
	}
	s.deleteObjectReplicas(ctx, obj)
	if err := s.Store.DeleteObject(ctx, bucket, key); err != nil {
		return err
	}
	_ = s.Store.AddProviderUsage(ctx, obj.ProviderAccountID, -obj.Size)
	s.dispatchObjectHook(ctx, domain.HookEventObjectDeleted, obj)
	return nil
}

func (s *Service) deleteObjectReplicas(ctx context.Context, obj domain.ObjectRecord) {
	replicas, err := s.Store.ListObjectReplicas(ctx, obj.Bucket, obj.Key)
	if err != nil {
		return
	}
	for _, replica := range replicas {
		account, err := s.Store.GetProvider(ctx, replica.ProviderAccountID)
		if err != nil {
			continue
		}
		account, adapter, err := s.providerForReplica(ctx, account)
		if err != nil {
			continue
		}
		replicaObj := domain.ObjectRecord{
			Bucket:            replica.Bucket,
			Key:               replica.Key,
			ProviderAccountID: replica.ProviderAccountID,
			RemoteBucket:      replica.RemoteBucket,
			RemoteKey:         replica.RemoteKey,
			Size:              replica.Size,
			ETag:              replica.ETag,
		}
		if err := adapter.Delete(ctx, account, replicaObj); err == nil {
			_ = s.Store.AddProviderUsage(ctx, replica.ProviderAccountID, -replica.Size)
		}
	}
	_ = s.Store.DeleteObjectReplicas(ctx, obj.Bucket, obj.Key)
}

func (s *Service) ListObjects(ctx context.Context, bucket, prefix string, limit int) ([]domain.ObjectRecord, error) {
	return s.ListObjectsAfter(ctx, bucket, prefix, "", limit)
}

func (s *Service) ListObjectsAfter(ctx context.Context, bucket, prefix, startAfter string, limit int) ([]domain.ObjectRecord, error) {
	if _, err := s.ensureBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return s.Store.ListObjectsAfter(ctx, bucket, prefix, startAfter, limit)
}

func (s *Service) UpsertProviderFromAdmin(ctx context.Context, account domain.ProviderAccount, secretKey string) error {
	if secretKey != "" {
		encrypted, err := s.Secrets.Encrypt(secretKey)
		if err != nil {
			return err
		}
		account.SecretEncrypted = encrypted
	} else if existing, err := s.Store.GetProvider(ctx, account.ID); err == nil {
		account.SecretEncrypted = existing.SecretEncrypted
	}
	return s.Store.UpsertProvider(ctx, account)
}

func (s *Service) putOnProvider(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	account, err := s.decryptAccount(account)
	if err != nil {
		return domain.StoredObject{}, err
	}
	adapter, ok := s.Providers.Get(account.Kind)
	if !ok {
		return domain.StoredObject{}, fmt.Errorf("provider kind %s is not registered", account.Kind)
	}
	return adapter.Put(ctx, account, input, body)
}

func (s *Service) providerForObject(ctx context.Context, obj domain.ObjectRecord) (domain.ProviderAccount, provider.Adapter, error) {
	account, err := s.Store.GetProvider(ctx, obj.ProviderAccountID)
	if err != nil {
		return domain.ProviderAccount{}, nil, err
	}
	return s.providerForReplica(ctx, account)
}

func (s *Service) providerForReplica(ctx context.Context, account domain.ProviderAccount) (domain.ProviderAccount, provider.Adapter, error) {
	account, err := s.decryptAccount(account)
	if err != nil {
		return domain.ProviderAccount{}, nil, err
	}
	adapter, ok := s.Providers.Get(account.Kind)
	if !ok {
		return domain.ProviderAccount{}, nil, fmt.Errorf("provider kind %s is not registered", account.Kind)
	}
	return account, adapter, nil
}

func (s *Service) decryptAccount(account domain.ProviderAccount) (domain.ProviderAccount, error) {
	if account.SecretEncrypted == "" {
		return account, nil
	}
	plain, err := s.Secrets.Decrypt(account.SecretEncrypted)
	if err != nil {
		return account, fmt.Errorf("decrypt provider %s secret: %w", account.ID, err)
	}
	account.SecretKey = plain
	return account, nil
}

func (s *Service) ensureBucket(ctx context.Context, name string) (domain.Bucket, error) {
	bucket, err := s.Store.GetBucket(ctx, name)
	if err == nil {
		return bucket, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		bucket = domain.Bucket{Name: name}
		return bucket, s.Store.UpsertBucket(ctx, bucket)
	}
	return domain.Bucket{}, err
}

func (s *Service) spoolUpload(body io.Reader) (io.ReadSeeker, func(), error) {
	file, err := os.CreateTemp(s.Config.Server.DataDir, "bucketmux-upload-*.tmp")
	if err != nil {
		return nil, nil, fmt.Errorf("create upload spool: %w", err)
	}
	cleanup := func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}
	if _, err := io.Copy(file, body); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("spool upload: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("rewind upload spool: %w", err)
	}
	return file, cleanup, nil
}

func (s *Service) replicateObject(ctx context.Context, input domain.PutObjectInput, primary domain.ObjectRecord, targets []string) string {
	successes := 0
	for _, providerID := range targets {
		status := "failed"
		errText := ""
		account, err := s.Store.GetProvider(ctx, providerID)
		if err != nil {
			errText = err.Error()
		} else if !account.Enabled {
			errText = "provider is disabled"
		} else if account.CapacityBytes > 0 && account.UsedBytes+input.Size > account.CapacityBytes {
			errText = "provider has insufficient capacity"
		} else {
			var body io.ReadCloser
			body, _, err = s.GetObject(ctx, primary.Bucket, primary.Key)
			if err == nil {
				var stored domain.StoredObject
				stored, err = s.putOnProvider(ctx, account, input, body)
				_ = body.Close()
				if err == nil {
					status = "succeeded"
					successes++
					_ = s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: primary.Bucket, Key: primary.Key, ProviderAccountID: account.ID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ETag: stored.ETag, Status: status})
					_ = s.Store.AddProviderUsage(ctx, account.ID, stored.Size)
					continue
				}
			}
			if err != nil {
				errText = err.Error()
			}
		}
		_ = s.Store.UpsertObjectReplica(ctx, domain.ObjectReplica{Bucket: primary.Bucket, Key: primary.Key, ProviderAccountID: providerID, Status: status, Error: errText})
	}
	if successes == len(targets) {
		return "completed"
	}
	if successes > 0 {
		return "partial"
	}
	return "failed"
}

func replicaTargets(bucket domain.Bucket, primaryProviderID string) []string {
	targets := normalizeProviderIDs(bucket.ReplicationProviderIDs)
	out := make([]string, 0, len(targets))
	for _, id := range targets {
		if id == primaryProviderID {
			continue
		}
		out = append(out, id)
	}
	return out
}

func normalizeProviderIDs(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}
