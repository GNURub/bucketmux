package wasmplugin

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/gnurub/bucketmux/internal/domain"
)

const (
	defaultMaxOperations = 16
	maxOperations        = 32
	maxOperationMapKeys  = 128
	maxOperationKeyBytes = 256
	maxOperationValBytes = 4096
)

// ValidateOperations validates and normalizes the complete command manifest
// before BucketMux applies any side effect. It is exported so alternate/test
// executors cannot accidentally bypass the same capability boundary.
func ValidateOperations(plugin domain.WASMPlugin, source domain.WASMPluginObject, operations []domain.WASMPluginOperation) error {
	if len(operations) == 0 {
		return nil
	}
	policy := plugin.OperationPolicy
	limit := policy.MaxOperations
	if limit <= 0 {
		limit = defaultMaxOperations
	}
	if limit > maxOperations {
		limit = maxOperations
	}
	if len(operations) > limit {
		return fmt.Errorf("plugin declared %d operations; policy maximum is %d", len(operations), limit)
	}
	allowed := make(map[string]bool, len(policy.AllowedOperations))
	for _, operationType := range policy.AllowedOperations {
		allowed[strings.TrimSpace(operationType)] = true
	}
	seenIDs := make(map[string]bool, len(operations))
	for i := range operations {
		op := &operations[i]
		op.ID = strings.TrimSpace(op.ID)
		if op.ID == "" {
			op.ID = fmt.Sprintf("operation-%d", i+1)
		}
		if len(op.ID) > 128 || seenIDs[op.ID] {
			return fmt.Errorf("operation %d has an invalid or duplicate id %q", i, op.ID)
		}
		seenIDs[op.ID] = true
		op.Type = strings.TrimSpace(op.Type)
		if !allowed[op.Type] {
			return fmt.Errorf("operation %q is not allowed by plugin policy", op.Type)
		}
		if err := normalizeOperation(op, source); err != nil {
			return fmt.Errorf("operation %q: %w", op.ID, err)
		}
		if err := validateOperationScope(policy, source, *op); err != nil {
			return fmt.Errorf("operation %q: %w", op.ID, err)
		}
	}
	return nil
}

func normalizeOperation(op *domain.WASMPluginOperation, source domain.WASMPluginObject) error {
	op.Bucket = strings.TrimSpace(op.Bucket)
	op.SourceBucket = strings.TrimSpace(op.SourceBucket)
	switch op.Type {
	case domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationTagsPatch, domain.WASMPluginOperationObjectDelete:
		if op.Bucket == "" {
			op.Bucket = source.Bucket
		}
		if op.Key == "" {
			op.Key = source.Key
		}
		key, err := normalizeObjectKey(op.Key)
		if err != nil {
			return err
		}
		op.Key = key
	case domain.WASMPluginOperationObjectCopy:
		if op.SourceBucket == "" {
			op.SourceBucket = source.Bucket
		}
		if op.SourceKey == "" {
			op.SourceKey = source.Key
		}
		if op.Bucket == "" {
			op.Bucket = source.Bucket
		}
		if strings.TrimSpace(op.Key) == "" {
			return fmt.Errorf("object.copy requires a target key")
		}
		var err error
		if op.SourceKey, err = normalizeObjectKey(op.SourceKey); err != nil {
			return fmt.Errorf("invalid source key: %w", err)
		}
		if op.Key, err = normalizeObjectKey(op.Key); err != nil {
			return fmt.Errorf("invalid target key: %w", err)
		}
		if op.SourceBucket == op.Bucket && op.SourceKey == op.Key {
			return fmt.Errorf("source and target must differ")
		}
	default:
		return fmt.Errorf("unsupported operation type %q", op.Type)
	}
	if op.Bucket == "" {
		return fmt.Errorf("bucket is required")
	}
	if err := validatePatch(op.Metadata); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := validatePatch(op.Tags); err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	var err error
	if op.RemoveMetadata, err = normalizedNames(op.RemoveMetadata); err != nil {
		return fmt.Errorf("remove_metadata: %w", err)
	}
	if op.RemoveTags, err = normalizedNames(op.RemoveTags); err != nil {
		return fmt.Errorf("remove_tags: %w", err)
	}
	return nil
}

func normalizeObjectKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("unsafe object key %q", value)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe object key %q", value)
	}
	return clean, nil
}

func validatePatch(values map[string]string) error {
	if len(values) > maxOperationMapKeys {
		return fmt.Errorf("contains %d keys; maximum is %d", len(values), maxOperationMapKeys)
	}
	for key, value := range values {
		if strings.TrimSpace(key) == "" || len(key) > maxOperationKeyBytes || len(value) > maxOperationValBytes {
			return fmt.Errorf("key %q or its value exceeds the allowed size", key)
		}
	}
	return nil
}

func normalizedNames(values []string) ([]string, error) {
	if len(values) > maxOperationMapKeys {
		return nil, fmt.Errorf("contains %d keys; maximum is %d", len(values), maxOperationMapKeys)
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxOperationKeyBytes {
			return nil, fmt.Errorf("key %q is empty or exceeds the allowed size", value)
		}
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result, nil
}

func validateOperationScope(policy domain.WASMPluginOperationPolicy, source domain.WASMPluginObject, op domain.WASMPluginOperation) error {
	objects := [][2]string{{op.Bucket, op.Key}}
	if op.Type == domain.WASMPluginOperationObjectCopy {
		objects = append(objects, [2]string{op.SourceBucket, op.SourceKey})
	}
	for _, object := range objects {
		bucket, key := object[0], object[1]
		if bucket == source.Bucket && key == source.Key {
			continue
		}
		if bucket != source.Bucket {
			matched := false
			for _, pattern := range policy.BucketPatterns {
				if ok, _ := path.Match(pattern, bucket); ok {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("bucket %q is outside the operation policy", bucket)
			}
		}
		if len(policy.KeyPrefixes) > 0 {
			matched := false
			for _, prefix := range policy.KeyPrefixes {
				if strings.HasPrefix(key, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("key %q is outside the operation policy", key)
			}
		}
	}
	return nil
}
