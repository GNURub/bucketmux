package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/gnurub/bucketmux/internal/wasmplugin"
)

type Service struct {
	Store       *store.Store
	Secrets     *secretcrypto.SecretBox
	Providers   *provider.Registry
	Router      *placement.PlacementRouter
	Config      config.Config
	WASMRuntime wasmplugin.Executor

	Coordinator     coordination.Coordinator
	WorkerLeaseTTL  time.Duration
	HookHTTPClient  *http.Client
	HookRetryDelay  func(attempts int) time.Duration
	hookWorkerWake  chan struct{}
	migrationWake   chan struct{}
	replicationWake chan struct{}
	inventoryWake   chan struct{}
	repairWake      chan struct{}
	wasmPluginWake  chan struct{}
	workerState     workerRuntimeState
	cancelWorkers   context.CancelFunc
	workerWG        sync.WaitGroup
}

var ErrUploadTooLarge = errors.New("upload exceeds configured size limit")

func NewService(ctx context.Context, cfg config.Config) (*Service, error) {
	cfg.Normalize()
	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.Server.MultipartStagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("create multipart staging dir: %w", err)
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
			provider.Entry(domain.ProviderKindAzureBlob, provider.NewAzureBlobAdapter()),
			provider.Entry(domain.ProviderKindCloudinary, provider.NewCloudinaryAdapter()),
			provider.Entry(domain.ProviderKindVercelBlob, provider.NewVercelBlobAdapter()),
		),
		Router:      placement.NewPlacementRouter(db),
		Config:      cfg,
		WASMRuntime: wasmplugin.NewRuntime(cfg.Server.DataDir),

		Coordinator:     coordinator,
		WorkerLeaseTTL:  time.Duration(cfg.Coordination.Redis.LeaseTTLSeconds) * time.Second,
		HookHTTPClient:  &http.Client{Timeout: defaultHookTimeout},
		hookWorkerWake:  make(chan struct{}, 1),
		migrationWake:   make(chan struct{}, 1),
		replicationWake: make(chan struct{}, 1),
		inventoryWake:   make(chan struct{}, 1),
		repairWake:      make(chan struct{}, 1),
		wasmPluginWake:  make(chan struct{}, 1),
		workerState:     newWorkerRuntimeState(),
		cancelWorkers:   cancelWorkers,
	}
	if err := svc.Bootstrap(ctx, cfg); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := svc.Store.RecoverExpiredProviderReservations(ctx, time.Now().UTC()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover provider reservations: %w", err)
	}
	svc.workerWG.Go(func() {
		svc.StartHookDeliveryWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartMigrationWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartReplicationWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartInventoryWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartRepairWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartLifecycleWorker(workerCtx)
	})
	svc.workerWG.Go(func() {
		svc.StartQuotaReconciliationWorker(workerCtx)
	})
	if cfg.WASMPlugins.Enabled {
		svc.workerWG.Go(func() {
			svc.StartWASMPluginWorker(workerCtx)
		})
	}
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
		bucket, err := s.Store.GetBucket(ctx, b.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("load bucket %s: %w", b.Name, err)
		}
		if errors.Is(err, store.ErrNotFound) {
			bucket = domain.Bucket{Name: b.Name}
		}
		bucket.ReplicationEnabled = len(replicationProviderIDs) > 0 || b.ReplicationEnabled
		bucket.ReplicationProviderIDs = replicationProviderIDs
		if b.VersioningEnabled != nil {
			bucket.VersioningEnabled = *b.VersioningEnabled
		}
		if b.TrashEnabled != nil {
			bucket.TrashEnabled = *b.TrashEnabled
		}
		if b.TrashRetentionDays != nil {
			bucket.TrashRetentionDays = *b.TrashRetentionDays
		}
		if b.ObjectLockEnabled != nil {
			bucket.ObjectLockEnabled = *b.ObjectLockEnabled
		}
		if b.DefaultRetentionMode != nil {
			bucket.DefaultRetentionMode = strings.ToUpper(*b.DefaultRetentionMode)
		}
		if b.DefaultRetentionDays != nil {
			bucket.DefaultRetentionDays = *b.DefaultRetentionDays
		}
		if b.LifecycleRules != nil {
			bucket.LifecycleRules = make([]domain.LifecycleRule, 0, len(b.LifecycleRules))
			for _, rule := range b.LifecycleRules {
				bucket.LifecycleRules = append(bucket.LifecycleRules, domain.LifecycleRule{ID: rule.ID, Prefix: rule.Prefix, ExpireAfterDays: rule.ExpireAfterDays, PurgeTrashAfterDays: rule.PurgeTrashAfterDays, Enabled: rule.Enabled})
			}
		}
		if err := s.Store.UpsertBucket(ctx, bucket); err != nil {
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
	if input.Size > s.Config.Server.MaxUploadBytes {
		return domain.ObjectRecord{}, fmt.Errorf("%w: maximum is %d bytes", ErrUploadTooLarge, s.Config.Server.MaxUploadBytes)
	}
	bucket, err := s.ensureBucket(ctx, input.Bucket)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	existing, existingErr := s.Store.GetObject(ctx, input.Bucket, input.Key)
	if existingErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &existing)
	}
	if bucket.VersioningEnabled {
		input.RemoteKey = ".bucketmux/versions/" + randomIdentifier("", 12) + "/" + strings.TrimLeft(input.Key, "/")
	}
	if bucket.ObjectLockEnabled && input.RetentionMode == "" && bucket.DefaultRetentionDays > 0 {
		input.RetentionMode = strings.ToUpper(bucket.DefaultRetentionMode)
		input.RetainUntil = time.Now().UTC().Add(time.Duration(bucket.DefaultRetentionDays) * 24 * time.Hour)
	}
	if (input.RetentionMode != "" || !input.RetainUntil.IsZero() || input.LegalHold) && !bucket.ObjectLockEnabled {
		return domain.ObjectRecord{}, fmt.Errorf("object lock is not enabled for bucket")
	}
	if input.RetentionMode != "" && input.RetentionMode != "GOVERNANCE" && input.RetentionMode != "COMPLIANCE" {
		return domain.ObjectRecord{}, fmt.Errorf("retention mode must be GOVERNANCE or COMPLIANCE")
	}
	if input.RetentionMode != "" && !input.RetainUntil.After(time.Now().UTC()) {
		return domain.ObjectRecord{}, fmt.Errorf("retain-until date must be in the future")
	}
	spool, cleanup, err := s.spoolUpload(body)
	if err != nil {
		return domain.ObjectRecord{}, err
	}
	defer cleanup()
	input.Size = spool.size
	input.ChecksumSHA256 = spool.checksumSHA256

	excluded := map[string]bool{}
	var primary domain.ProviderAccount
	var stored domain.StoredObject
	for {
		primary, err = s.Router.Choose(ctx, input, excluded)
		if err != nil {
			return domain.ObjectRecord{}, err
		}
		reservation := domain.ProviderReservation{ID: randomIdentifier("res-", 12), ProviderAccountID: primary.ID, Bytes: input.Size, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		margin := intSetting(primary.Settings, "quota_margin_bytes")
		if minFree := intSetting(primary.Settings, "min_free_bytes"); minFree > margin {
			margin = minFree
		}
		reserved, reserveErr := s.Store.ReserveProviderCapacity(ctx, reservation, margin, intSetting(primary.Settings, "monthly_upload_quota_bytes"), time.Now().UTC().Format("2006-01"))
		if reserveErr != nil {
			return domain.ObjectRecord{}, fmt.Errorf("reserve provider capacity: %w", reserveErr)
		}
		if !reserved {
			excluded[primary.ID] = true
			continue
		}
		if err := spool.rewind(); err != nil {
			_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
			return domain.ObjectRecord{}, err
		}
		stored, err = s.putOnProvider(ctx, primary, input, spool.file)
		if err != nil {
			_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
			s.recordProviderWriteFailure(ctx, primary, err)
			if provider.FailoverEligible(err) {
				excluded[primary.ID] = true
				continue
			}
			return domain.ObjectRecord{}, err
		}
		if err := s.Store.CommitProviderReservation(ctx, reservation.ID, stored.Size); err != nil {
			_ = s.Store.ReleaseProviderReservation(ctx, reservation.ID)
			if account, adapter, adapterErr := s.providerForReplica(ctx, primary); adapterErr == nil {
				_ = adapter.Delete(ctx, account, domain.ObjectRecord{ProviderAccountID: primary.ID, RemoteBucket: stored.RemoteBucket, RemoteKey: stored.RemoteKey, Size: stored.Size})
			}
			return domain.ObjectRecord{}, fmt.Errorf("commit provider reservation: %w", err)
		}
		break
	}
	if stored.ChecksumSHA256 == "" {
		stored.ChecksumSHA256 = input.ChecksumSHA256
	}
	targets := replicaTargets(bucket, primary.ID)
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
		Metadata:          input.Metadata,
		Tags:              input.Tags,
		RetentionMode:     input.RetentionMode,
		RetainUntil:       input.RetainUntil,
		LegalHold:         input.LegalHold,
		ReplicaStatus:     replicaStatus,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if bucket.VersioningEnabled {
		obj.VersionID = randomIdentifier("v-", 12)
	}
	if existingErr == nil && bucket.VersioningEnabled {
		if existing.VersionID == "" {
			existing.VersionID = "null"
		}
		if err := s.Store.PutObjectVersion(ctx, existing); err != nil {
			return domain.ObjectRecord{}, err
		}
	}
	if err := s.Store.PutObject(ctx, obj); err != nil {
		return domain.ObjectRecord{}, err
	}
	// An overwrite creates a new object generation. Never expose embeddings
	// produced from the previous bytes while the new plugin job is pending.
	if err := s.Store.DeleteObjectEmbeddings(ctx, obj.Bucket, obj.Key); err != nil {
		return domain.ObjectRecord{}, fmt.Errorf("invalidate object embeddings: %w", err)
	}
	if err := s.Store.PutObjectAttributes(ctx, obj); err != nil {
		return domain.ObjectRecord{}, err
	}
	if bucket.VersioningEnabled {
		if err := s.Store.PutObjectVersion(ctx, obj); err != nil {
			return domain.ObjectRecord{}, err
		}
	}
	if existingErr == nil && !bucket.VersioningEnabled {
		_ = s.cleanupOverwrittenObject(ctx, existing, obj)
	}
	if existingErr == nil {
		s.deleteObjectReplicas(ctx, existing)
	}
	if len(targets) > 0 {
		_ = s.enqueueObjectReplicas(ctx, obj, targets)
	}
	s.dispatchObjectHook(ctx, domain.HookEventObjectCreated, obj)
	if s.Config.WASMPlugins.Enabled && !input.SkipWASMPipelines {
		if err := s.enqueueWASMPlugins(ctx, domain.WASMPluginEventObjectCreated, obj); err != nil {
			s.recordWorkerFailure("wasm-plugins", fmt.Errorf("enqueue object %s/%s: %w", obj.Bucket, obj.Key, err))
		}
	}
	return obj, nil
}

func (s *Service) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, domain.ObjectRecord, error) {
	obj, account, err := s.Store.GetObjectWithProvider(ctx, bucket, key)
	if err != nil {
		return nil, obj, err
	}
	account, adapter, primaryErr := s.providerForReplica(ctx, account)
	if primaryErr == nil {
		body, served, getErr := adapter.Get(ctx, account, obj)
		if getErr == nil {
			_ = s.Store.HydrateObjectAttributes(ctx, &served)
			return body, served, nil
		}
		primaryErr = getErr
	}
	body, served, fallbackErr := s.getObjectFromReplica(ctx, obj)
	if fallbackErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &served)
		return body, served, nil
	}
	return nil, obj, fmt.Errorf("read primary and replicas: %w", errors.Join(primaryErr, fallbackErr))
}

func (s *Service) HeadObject(ctx context.Context, bucket, key string) (domain.ObjectRecord, error) {
	obj, account, err := s.Store.GetObjectWithProvider(ctx, bucket, key)
	if err != nil {
		return obj, err
	}
	account, adapter, primaryErr := s.providerForReplica(ctx, account)
	if primaryErr == nil {
		head, headErr := adapter.Head(ctx, account, obj)
		if headErr == nil {
			_ = s.Store.HydrateObjectAttributes(ctx, &head)
			return head, nil
		}
		primaryErr = headErr
	}
	head, fallbackErr := s.headObjectFromReplica(ctx, obj)
	if fallbackErr == nil {
		_ = s.Store.HydrateObjectAttributes(ctx, &head)
		return head, nil
	}
	return obj, fmt.Errorf("head primary and replicas: %w", errors.Join(primaryErr, fallbackErr))
}

func (s *Service) getObjectFromReplica(ctx context.Context, primary domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	replicas, err := s.Store.ListObjectReplicas(ctx, primary.Bucket, primary.Key)
	if err != nil {
		return nil, primary, err
	}
	var failures []error
	for _, replica := range replicas {
		if replica.Status != replicaStatusSucceeded {
			continue
		}
		account, err := s.Store.GetProvider(ctx, replica.ProviderAccountID)
		if err != nil {
			failures = append(failures, fmt.Errorf("replica %s unavailable: %w", replica.ProviderAccountID, err))
			continue
		}
		if !account.Enabled {
			failures = append(failures, fmt.Errorf("replica %s is disabled", replica.ProviderAccountID))
			continue
		}
		account, adapter, err := s.providerForReplica(ctx, account)
		if err != nil {
			failures = append(failures, fmt.Errorf("replica %s: %w", replica.ProviderAccountID, err))
			continue
		}
		candidate := objectRecordForReplica(primary, replica)
		body, served, err := adapter.Get(ctx, account, candidate)
		if err == nil {
			return body, served, nil
		}
		failures = append(failures, fmt.Errorf("replica %s: %w", replica.ProviderAccountID, err))
	}
	if len(failures) == 0 {
		return nil, primary, errors.New("no completed replicas are available")
	}
	return nil, primary, errors.Join(failures...)
}

func (s *Service) headObjectFromReplica(ctx context.Context, primary domain.ObjectRecord) (domain.ObjectRecord, error) {
	replicas, err := s.Store.ListObjectReplicas(ctx, primary.Bucket, primary.Key)
	if err != nil {
		return primary, err
	}
	var failures []error
	for _, replica := range replicas {
		if replica.Status != replicaStatusSucceeded {
			continue
		}
		account, err := s.Store.GetProvider(ctx, replica.ProviderAccountID)
		if err != nil {
			failures = append(failures, fmt.Errorf("replica %s unavailable: %w", replica.ProviderAccountID, err))
			continue
		}
		if !account.Enabled {
			failures = append(failures, fmt.Errorf("replica %s is disabled", replica.ProviderAccountID))
			continue
		}
		account, adapter, err := s.providerForReplica(ctx, account)
		if err != nil {
			failures = append(failures, fmt.Errorf("replica %s: %w", replica.ProviderAccountID, err))
			continue
		}
		candidate := objectRecordForReplica(primary, replica)
		head, err := adapter.Head(ctx, account, candidate)
		if err == nil {
			return head, nil
		}
		failures = append(failures, fmt.Errorf("replica %s: %w", replica.ProviderAccountID, err))
	}
	if len(failures) == 0 {
		return primary, errors.New("no completed replicas are available")
	}
	return primary, errors.Join(failures...)
}

func objectRecordForReplica(primary domain.ObjectRecord, replica domain.ObjectReplica) domain.ObjectRecord {
	primary.ProviderAccountID = replica.ProviderAccountID
	primary.RemoteBucket = replica.RemoteBucket
	primary.RemoteKey = replica.RemoteKey
	if replica.Size > 0 {
		primary.Size = replica.Size
	}
	if replica.ETag != "" {
		primary.ETag = replica.ETag
	}
	if replica.ChecksumSHA256 != "" {
		primary.ChecksumSHA256 = replica.ChecksumSHA256
	}
	return primary
}

func (s *Service) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := s.DeleteObjectWithOptions(ctx, bucket, key, DeleteObjectOptions{})
	return err
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
	validation, err := s.ValidateProvider(ctx, account)
	if err != nil {
		return err
	}
	if account.Enabled && validation.Health.Status != domain.ProviderHealthHealthy {
		return fmt.Errorf("provider cannot be enabled before onboarding succeeds: %s", validation.Health.Message)
	}
	if err := s.Store.UpsertProvider(ctx, account); err != nil {
		return err
	}
	if account.Enabled {
		s.recordProviderHealth(ctx, account, validation.Health)
	}
	return nil
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

type uploadSpool struct {
	file           *os.File
	size           int64
	checksumSHA256 string
}

func (spool *uploadSpool) rewind() error {
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind upload spool: %w", err)
	}
	return nil
}

func (s *Service) spoolUpload(body io.Reader) (*uploadSpool, func(), error) {
	file, err := os.CreateTemp(s.Config.Server.DataDir, "bucketmux-upload-*.tmp")
	if err != nil {
		return nil, nil, fmt.Errorf("create upload spool: %w", err)
	}
	cleanup := func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}
	hash := sha256.New()
	written, err := io.Copy(file, io.TeeReader(io.LimitReader(body, s.Config.Server.MaxUploadBytes+1), hash))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("spool upload: %w", err)
	}
	if written > s.Config.Server.MaxUploadBytes {
		cleanup()
		return nil, nil, fmt.Errorf("%w: maximum is %d bytes", ErrUploadTooLarge, s.Config.Server.MaxUploadBytes)
	}
	spool := &uploadSpool{file: file, size: written, checksumSHA256: hex.EncodeToString(hash.Sum(nil))}
	if err := spool.rewind(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return spool, cleanup, nil
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
