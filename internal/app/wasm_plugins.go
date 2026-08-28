package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/store"
	"github.com/gnurub/bucketmux/internal/wasmplugin"
)

const (
	wasmPluginWorkerInterval = 2 * time.Second
	wasmPluginHeartbeat      = 10 * time.Second
	wasmPluginWorkStaleAfter = 2 * time.Minute
	wasmPluginMaxTimeout     = 10 * time.Minute
	wasmPluginMaxMemoryBytes = int64(1 << 30)
	wasmPluginMaxOperations  = 32
)

var errWASMSourceSuperseded = errors.New("source object was replaced during plugin execution")

func (s *Service) UpsertWASMPlugin(ctx context.Context, plugin domain.WASMPlugin) error {
	if err := s.normalizeWASMPlugin(&plugin); err != nil {
		return err
	}
	if int64(len(plugin.ModuleBase64)) > s.Config.WASMPlugins.MaxModuleBytes*4/3+4 {
		return fmt.Errorf("base64 wasm module exceeds the configured module limit")
	}
	module, err := base64.StdEncoding.DecodeString(plugin.ModuleBase64)
	if err != nil {
		return fmt.Errorf("decode wasm module: %w", err)
	}
	if int64(len(module)) > s.Config.WASMPlugins.MaxModuleBytes {
		return fmt.Errorf("wasm module exceeds %d bytes", s.Config.WASMPlugins.MaxModuleBytes)
	}
	digest := sha256.Sum256(module)
	plugin.ModuleSHA256 = hex.EncodeToString(digest[:])
	if err := s.validateWASMModule(ctx, module, plugin); err != nil {
		return err
	}
	return s.Store.UpsertWASMPlugin(ctx, plugin)
}

func (s *Service) ValidateWASMPlugin(ctx context.Context, plugin domain.WASMPlugin) error {
	if err := s.normalizeWASMPlugin(&plugin); err != nil {
		return err
	}
	if int64(len(plugin.ModuleBase64)) > s.Config.WASMPlugins.MaxModuleBytes*4/3+4 {
		return fmt.Errorf("base64 wasm module exceeds the configured module limit")
	}
	module, err := base64.StdEncoding.DecodeString(plugin.ModuleBase64)
	if err != nil {
		return fmt.Errorf("decode wasm module: %w", err)
	}
	if int64(len(module)) > s.Config.WASMPlugins.MaxModuleBytes {
		return fmt.Errorf("wasm module exceeds %d bytes", s.Config.WASMPlugins.MaxModuleBytes)
	}
	return s.validateWASMModule(ctx, module, plugin)
}

func (s *Service) validateWASMModule(ctx context.Context, module []byte, plugin domain.WASMPlugin) error {
	timeout := min(time.Duration(plugin.TimeoutMillis)*time.Millisecond, 30*time.Second)
	validationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.WASMRuntime.Validate(validationCtx, module, plugin)
}

func (s *Service) normalizeWASMPlugin(plugin *domain.WASMPlugin) error {
	plugin.ID = strings.TrimSpace(plugin.ID)
	plugin.Name = strings.TrimSpace(plugin.Name)
	plugin.ABIVersion = strings.TrimSpace(plugin.ABIVersion)
	plugin.BucketPattern = strings.TrimSpace(plugin.BucketPattern)
	plugin.KeyPrefix = strings.TrimLeft(strings.TrimSpace(plugin.KeyPrefix), "/")
	plugin.KeySuffix = strings.TrimSpace(plugin.KeySuffix)
	if plugin.ID == "" || plugin.Name == "" || plugin.ModuleBase64 == "" {
		return errors.New("plugin id, name and module_base64 are required")
	}
	if plugin.ABIVersion == "" {
		plugin.ABIVersion = domain.WASMPluginABIV1
	}
	if plugin.ABIVersion != domain.WASMPluginABIV1 {
		return fmt.Errorf("unsupported wasm ABI %q", plugin.ABIVersion)
	}
	if plugin.BucketPattern == "" {
		plugin.BucketPattern = "*"
	}
	if _, err := path.Match(plugin.BucketPattern, "bucket"); err != nil {
		return fmt.Errorf("invalid bucket pattern: %w", err)
	}
	if len(plugin.Events) == 0 {
		plugin.Events = []string{domain.WASMPluginEventObjectCreated}
	}
	for _, event := range plugin.Events {
		if event != domain.WASMPluginEventObjectCreated {
			return fmt.Errorf("unsupported plugin event %q", event)
		}
	}
	if plugin.TimeoutMillis <= 0 {
		plugin.TimeoutMillis = s.Config.WASMPlugins.DefaultTimeoutMillis
	}
	if plugin.MemoryLimitBytes <= 0 {
		plugin.MemoryLimitBytes = s.Config.WASMPlugins.DefaultMemoryLimitBytes
	}
	if plugin.MaxInputBytes <= 0 {
		plugin.MaxInputBytes = min(s.Config.WASMPlugins.DefaultMaxInputBytes, s.Config.Server.MaxUploadBytes)
	}
	if plugin.MaxOutputBytes <= 0 {
		plugin.MaxOutputBytes = min(s.Config.WASMPlugins.DefaultMaxOutputBytes, s.Config.Server.MaxUploadBytes)
	}
	if plugin.MaxAttempts <= 0 {
		plugin.MaxAttempts = s.Config.WASMPlugins.DefaultMaxAttempts
	}
	if time.Duration(plugin.TimeoutMillis)*time.Millisecond > wasmPluginMaxTimeout {
		return fmt.Errorf("plugin timeout cannot exceed %s", wasmPluginMaxTimeout)
	}
	if plugin.MemoryLimitBytes > wasmPluginMaxMemoryBytes {
		return fmt.Errorf("plugin memory limit cannot exceed %d bytes", wasmPluginMaxMemoryBytes)
	}
	if plugin.MaxInputBytes > s.Config.Server.MaxUploadBytes || plugin.MaxOutputBytes > s.Config.Server.MaxUploadBytes {
		return fmt.Errorf("plugin input and output limits cannot exceed server.max_upload_bytes")
	}
	if plugin.Config == nil {
		plugin.Config = map[string]string{}
	}
	plugin.Events = uniqueStrings(plugin.Events)
	plugin.ContentTypes = uniqueStrings(plugin.ContentTypes)
	plugin.OperationPolicy.AllowedOperations = uniqueStrings(plugin.OperationPolicy.AllowedOperations)
	for _, operationType := range plugin.OperationPolicy.AllowedOperations {
		switch operationType {
		case domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationTagsPatch, domain.WASMPluginOperationObjectCopy, domain.WASMPluginOperationObjectDelete:
		default:
			return fmt.Errorf("unsupported plugin operation %q", operationType)
		}
	}
	plugin.OperationPolicy.BucketPatterns = uniqueStrings(plugin.OperationPolicy.BucketPatterns)
	for _, pattern := range plugin.OperationPolicy.BucketPatterns {
		if _, err := path.Match(pattern, "bucket"); err != nil {
			return fmt.Errorf("invalid operation bucket pattern %q: %w", pattern, err)
		}
	}
	plugin.OperationPolicy.KeyPrefixes = uniqueStrings(plugin.OperationPolicy.KeyPrefixes)
	for i, prefix := range plugin.OperationPolicy.KeyPrefixes {
		prefix = strings.TrimLeft(strings.TrimSpace(prefix), "/")
		if prefix == "" || prefix == "." || prefix == ".." || strings.HasPrefix(path.Clean(prefix), "../") {
			return fmt.Errorf("invalid operation key prefix %q", prefix)
		}
		plugin.OperationPolicy.KeyPrefixes[i] = prefix
	}
	if plugin.OperationPolicy.MaxOperations <= 0 {
		plugin.OperationPolicy.MaxOperations = 16
	}
	if plugin.OperationPolicy.MaxOperations > wasmPluginMaxOperations {
		return fmt.Errorf("plugin operation maximum cannot exceed %d", wasmPluginMaxOperations)
	}
	return nil
}

func (s *Service) enqueueWASMPlugins(ctx context.Context, event string, object domain.ObjectRecord) error {
	// Store.PutObject owns the authoritative updated_at timestamp. Reload it so
	// identical-byte overwrites still create a distinct, race-detectable job.
	persisted, err := s.Store.GetObject(ctx, object.Bucket, object.Key)
	if err != nil {
		return err
	}
	object.CreatedAt = persisted.CreatedAt
	object.UpdatedAt = persisted.UpdatedAt
	plugins, err := s.Store.ListWASMPlugins(ctx, true)
	if err != nil {
		return err
	}
	enqueued := false
	for _, plugin := range plugins {
		if !wasmPluginMatches(plugin, event, object) {
			continue
		}
		sourceIdentity := object.ChecksumSHA256 + "\x00" + object.ETag + "\x00" + object.UpdatedAt.UTC().Format(time.RFC3339Nano)
		dedupeSum := sha256.Sum256([]byte(strings.Join([]string{plugin.ID, event, object.Bucket, object.Key, sourceIdentity}, "\x00")))
		created, err := s.Store.CreateWASMPluginJob(ctx, domain.WASMPluginJob{
			ID:              randomIdentifier("wasm-job-", 12),
			PluginID:        plugin.ID,
			Event:           event,
			Bucket:          object.Bucket,
			Key:             object.Key,
			SourceChecksum:  object.ChecksumSHA256,
			SourceUpdatedAt: object.UpdatedAt,
			DedupeKey:       hex.EncodeToString(dedupeSum[:]),
			MaxAttempts:     plugin.MaxAttempts,
		})
		if err != nil {
			return err
		}
		enqueued = enqueued || created
	}
	if enqueued {
		signalWorker(s.wasmPluginWake)
	}
	return nil
}

func wasmPluginMatches(plugin domain.WASMPlugin, event string, object domain.ObjectRecord) bool {
	if !slices.Contains(plugin.Events, event) || !strings.HasPrefix(object.Key, plugin.KeyPrefix) || !strings.HasSuffix(object.Key, plugin.KeySuffix) {
		return false
	}
	matched, err := path.Match(plugin.BucketPattern, object.Bucket)
	if err != nil || !matched {
		return false
	}
	if len(plugin.ContentTypes) == 0 {
		return true
	}
	contentType, _, _ := mime.ParseMediaType(object.ContentType)
	for _, pattern := range plugin.ContentTypes {
		if pattern == "*/*" || strings.EqualFold(pattern, contentType) {
			return true
		}
		if strings.HasSuffix(pattern, "/*") && strings.HasPrefix(strings.ToLower(contentType), strings.TrimSuffix(strings.ToLower(pattern), "*")) {
			return true
		}
	}
	return false
}

func (s *Service) StartWASMPluginWorker(ctx context.Context) {
	s.runDurableWorker(ctx, durableWorker{
		name:              "wasm-plugins",
		interval:          wasmPluginWorkerInterval,
		heartbeatInterval: wasmPluginHeartbeat,
		staleAfter:        wasmPluginWorkStaleAfter,
		wake:              s.wasmPluginWake,
		recover: func(ctx context.Context, cutoff time.Time) error {
			_, err := s.Store.RecoverStaleWASMPluginJobs(ctx, cutoff)
			return err
		},
		claim: func(ctx context.Context) (durableWorkItem, bool, error) {
			job, claimed, err := s.Store.ClaimNextWASMPluginJob(ctx, time.Now().UTC())
			if err != nil || !claimed {
				return durableWorkItem{}, claimed, err
			}
			return durableWorkItem{
				run: func(ctx context.Context) error {
					return s.runWASMPluginJob(ctx, job)
				},
				heartbeat: func(ctx context.Context) error {
					return s.Store.TouchWASMPluginJob(ctx, job.ID)
				},
			}, true, nil
		},
	})
}

func (s *Service) runWASMPluginJob(ctx context.Context, job domain.WASMPluginJob) error {
	plugin, err := s.Store.GetWASMPlugin(ctx, job.PluginID)
	if err != nil {
		return s.failWASMPluginJob(ctx, job, fmt.Errorf("load plugin: %w", err), true)
	}
	if !plugin.Enabled {
		return s.failWASMPluginJob(ctx, job, errors.New("plugin is disabled"), true)
	}
	module, err := base64.StdEncoding.DecodeString(plugin.ModuleBase64)
	if err != nil {
		return s.failWASMPluginJob(ctx, job, fmt.Errorf("decode plugin module: %w", err), true)
	}
	object, err := s.Store.GetObject(ctx, job.Bucket, job.Key)
	if err != nil {
		return s.failWASMPluginJob(ctx, job, fmt.Errorf("load source object: %w", err), true)
	}
	_ = s.Store.HydrateObjectAttributes(ctx, &object)
	if job.SourceChecksum != "" && object.ChecksumSHA256 != job.SourceChecksum {
		job.Status = domain.WASMPluginStatusSuperseded
		job.LastError = "source object was replaced before plugin execution"
		job.FinishedAt = time.Now().UTC()
		return s.Store.UpdateWASMPluginJob(ctx, job)
	}
	if !job.SourceUpdatedAt.IsZero() && !object.UpdatedAt.Equal(job.SourceUpdatedAt) {
		job.Status = domain.WASMPluginStatusSuperseded
		job.LastError = "source object was replaced before plugin execution"
		job.FinishedAt = time.Now().UTC()
		return s.Store.UpdateWASMPluginJob(ctx, job)
	}
	body, _, err := s.GetObject(ctx, object.Bucket, object.Key)
	if err != nil {
		return s.failWASMPluginJob(ctx, job, fmt.Errorf("read source object: %w", err), false)
	}
	invocation := domain.WASMPluginInvocation{
		Event: job.Event,
		JobID: job.ID,
		Object: domain.WASMPluginObject{
			Bucket: object.Bucket, Key: object.Key, Size: object.Size, ContentType: object.ContentType,
			ETag: object.ETag, ChecksumSHA256: object.ChecksumSHA256, Metadata: object.Metadata, Tags: object.Tags,
		},
		Config:       plugin.Config,
		Capabilities: domain.WASMPluginCapabilities{Operations: plugin.OperationPolicy},
	}
	execution, executeErr := s.WASMRuntime.Execute(ctx, module, plugin, invocation, body)
	closeErr := body.Close()
	if executeErr != nil {
		return s.failWASMPluginJob(ctx, job, executeErr, false)
	}
	defer func() { _ = execution.Close() }()
	if closeErr != nil {
		return s.failWASMPluginJob(ctx, job, closeErr, false)
	}
	if err := s.applyWASMPluginResult(ctx, plugin, object, execution); err != nil {
		if errors.Is(err, errWASMSourceSuperseded) {
			job.Status = domain.WASMPluginStatusSuperseded
			job.LastError = err.Error()
			job.FinishedAt = time.Now().UTC()
			return s.Store.UpdateWASMPluginJob(ctx, job)
		}
		return s.failWASMPluginJob(ctx, job, err, false)
	}
	publicResult := execution.Result
	publicResult.Embeddings = slices.Clone(execution.Result.Embeddings)
	for i := range publicResult.Embeddings {
		publicResult.Embeddings[i].Values = nil
	}
	resultJSON, err := json.Marshal(publicResult)
	if err != nil {
		return s.failWASMPluginJob(ctx, job, err, true)
	}
	job.Status = domain.WASMPluginStatusSucceeded
	job.LastError = ""
	job.ResultJSON = string(resultJSON)
	job.FinishedAt = time.Now().UTC()
	_ = s.Store.ResolveAlert(ctx, wasmPluginAlertKey(job))
	return s.Store.UpdateWASMPluginJob(ctx, job)
}

func (s *Service) applyWASMPluginResult(ctx context.Context, plugin domain.WASMPlugin, source domain.ObjectRecord, execution *wasmplugin.Execution) error {
	if err := wasmplugin.ValidateOperations(plugin, domain.WASMPluginObject{Bucket: source.Bucket, Key: source.Key}, execution.Result.Operations); err != nil {
		return fmt.Errorf("validate plugin operations: %w", err)
	}
	current, err := s.Store.GetObject(ctx, source.Bucket, source.Key)
	if err != nil {
		return fmt.Errorf("reload source object: %w", err)
	}
	if (source.ChecksumSHA256 != "" && current.ChecksumSHA256 != source.ChecksumSHA256) || !current.UpdatedAt.Equal(source.UpdatedAt) {
		return errWASMSourceSuperseded
	}
	_ = s.Store.HydrateObjectAttributes(ctx, &current)
	if err := s.Store.ReplaceObjectEmbeddings(ctx, current, plugin.ID, execution.Result.Embeddings); err != nil {
		return fmt.Errorf("store plugin embeddings: %w", err)
	}
	current.Metadata = mergeStringMaps(current.Metadata, execution.Result.Metadata)
	current.Tags = mergeStringMaps(current.Tags, execution.Result.Tags)
	if err := s.Store.PutObjectAttributes(ctx, current); err != nil {
		return fmt.Errorf("store plugin metadata: %w", err)
	}
	for _, derived := range execution.Result.DerivedObjects {
		derivedKey := strings.TrimLeft(strings.TrimSpace(derived.Key), "/")
		if derivedKey == "" {
			if derived.KeySuffix != "" {
				derivedKey = source.Key + derived.KeySuffix
			} else {
				derivedKey = source.Key + ".bucketmux-derived/" + plugin.ID + "/" + path.Base(derived.Path)
			}
		}
		cleanKey := path.Clean(derivedKey)
		if cleanKey == "." || cleanKey == ".." || strings.HasPrefix(cleanKey, "../") || cleanKey == source.Key {
			return fmt.Errorf("unsafe derived object key %q", derivedKey)
		}
		file, err := execution.OpenDerived(derived.Path)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		metadata := mergeStringMaps(derived.Metadata, map[string]string{
			"bucketmux-derived-from": source.Key,
			"bucketmux-wasm-plugin":  plugin.ID,
		})
		_, putErr := s.PutObject(ctx, domain.PutObjectInput{
			Bucket: source.Bucket, Key: cleanKey, Size: info.Size(), ContentType: derived.ContentType,
			Metadata: metadata, Tags: derived.Tags, SkipWASMPipelines: true,
		}, file)
		closeErr := file.Close()
		if putErr != nil {
			return fmt.Errorf("store derived object %q: %w", cleanKey, putErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, operation := range execution.Result.Operations {
		if err := s.applyWASMPluginOperation(ctx, operation); err != nil {
			return fmt.Errorf("apply operation %q (%s): %w", operation.ID, operation.Type, err)
		}
		s.RecordAuditEvent(ctx, domain.AuditEvent{
			Actor: "wasm:" + plugin.ID, Action: domain.AuditActionWASMOperation,
			Bucket: operation.Bucket, Key: operation.Key, TargetID: operation.ID, Detail: operation.Type,
		})
	}
	return nil
}

func (s *Service) applyWASMPluginOperation(ctx context.Context, operation domain.WASMPluginOperation) error {
	switch operation.Type {
	case domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationTagsPatch:
		object, err := s.Store.GetObject(ctx, operation.Bucket, operation.Key)
		if err != nil {
			return err
		}
		if err := s.Store.HydrateObjectAttributes(ctx, &object); err != nil {
			return err
		}
		if operation.Type == domain.WASMPluginOperationMetadataPatch {
			object.Metadata = patchStringMap(object.Metadata, operation.Metadata, operation.RemoveMetadata)
		} else {
			object.Tags = patchStringMap(object.Tags, operation.Tags, operation.RemoveTags)
		}
		return s.Store.PutObjectAttributes(ctx, object)
	case domain.WASMPluginOperationObjectCopy:
		targetBucket, err := s.Store.GetBucket(ctx, operation.Bucket)
		if err != nil {
			return fmt.Errorf("load target bucket: %w", err)
		}
		if !targetBucket.VersioningEnabled {
			existing, existingErr := s.Store.GetObject(ctx, operation.Bucket, operation.Key)
			if existingErr == nil {
				_ = s.Store.HydrateObjectAttributes(ctx, &existing)
				if err := validateObjectDeletion(existing, DeleteObjectOptions{}); err != nil {
					return fmt.Errorf("copy target is protected: %w", err)
				}
			} else if !errors.Is(existingErr, store.ErrNotFound) {
				return existingErr
			}
		}
		body, source, err := s.GetObject(ctx, operation.SourceBucket, operation.SourceKey)
		if err != nil {
			return fmt.Errorf("read copy source: %w", err)
		}
		metadata := patchStringMap(source.Metadata, operation.Metadata, operation.RemoveMetadata)
		tags := patchStringMap(source.Tags, operation.Tags, operation.RemoveTags)
		_, putErr := s.PutObject(ctx, domain.PutObjectInput{
			Bucket: operation.Bucket, Key: operation.Key, Size: source.Size, ContentType: source.ContentType,
			Metadata: metadata, Tags: tags, ChecksumSHA256: source.ChecksumSHA256, SkipWASMPipelines: true,
		}, body)
		closeErr := body.Close()
		if putErr != nil {
			return putErr
		}
		return closeErr
	case domain.WASMPluginOperationObjectDelete:
		_, err := s.DeleteObjectWithOptions(ctx, operation.Bucket, operation.Key, DeleteObjectOptions{})
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unsupported operation type %q", operation.Type)
	}
}

func patchStringMap(base, additions map[string]string, removals []string) map[string]string {
	patched := mergeStringMaps(base, additions)
	for _, key := range removals {
		delete(patched, key)
	}
	return patched
}

func (s *Service) ListObjectEmbeddings(ctx context.Context, bucket, key string) ([]domain.ObjectEmbedding, error) {
	if _, err := s.Store.GetObject(ctx, bucket, key); err != nil {
		return nil, err
	}
	return s.Store.ListObjectEmbeddings(ctx, bucket, key, false)
}

func (s *Service) SearchObjectEmbeddings(ctx context.Context, query domain.EmbeddingSearchQuery) ([]domain.EmbeddingSearchResult, error) {
	return s.Store.SearchObjectEmbeddings(ctx, query)
}

func (s *Service) failWASMPluginJob(ctx context.Context, job domain.WASMPluginJob, cause error, permanent bool) error {
	job.LastError = cause.Error()
	if !permanent && job.Attempts < job.MaxAttempts {
		job.Status = domain.WASMPluginStatusPending
		job.NextAttemptAt = time.Now().UTC().Add(time.Duration(1<<min(job.Attempts, 8)) * time.Second)
	} else {
		job.Status = domain.WASMPluginStatusFailed
		job.FinishedAt = time.Now().UTC()
		s.raiseAlert(ctx, domain.Alert{DedupeKey: wasmPluginAlertKey(job), Type: domain.AlertTypeWASMPluginFailed, Severity: domain.AlertSeverityWarning, Bucket: job.Bucket, Key: job.Key, Message: cause.Error()})
	}
	_ = s.Store.UpdateWASMPluginJob(ctx, job)
	return cause
}

func wasmPluginAlertKey(job domain.WASMPluginJob) string {
	return "wasm-plugin:" + job.PluginID + ":" + job.Bucket + ":" + job.Key
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func mergeStringMaps(base, additions map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(additions))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range additions {
		merged[key] = value
	}
	return merged
}
