package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"golang.org/x/sys/unix"
)

type LocalAdapter struct {
	baseDir string
}

func NewLocalAdapter(baseDir string) *LocalAdapter {
	return &LocalAdapter{baseDir: baseDir}
}

func (a *LocalAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	remoteKey, err := safeRelativePath(input.StorageKey())
	if err != nil {
		return domain.StoredObject{}, err
	}
	path, err := a.objectPath(account, input.Bucket, remoteKey)
	if err != nil {
		return domain.StoredObject{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return domain.StoredObject{}, fmt.Errorf("create local object dir: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".bucketmux-upload-*.tmp")
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("create local object temp file: %w", err)
	}
	tempPath := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(file, io.TeeReader(body, hash))
	if err != nil {
		if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
			return domain.StoredObject{}, &Error{Op: "put local object", Kind: FailureQuota, Err: err}
		}
		return domain.StoredObject{}, fmt.Errorf("write local object: %w", err)
	}
	if err := file.Sync(); err != nil {
		return domain.StoredObject{}, fmt.Errorf("sync local object: %w", err)
	}
	if err := file.Close(); err != nil {
		return domain.StoredObject{}, fmt.Errorf("close local object: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return domain.StoredObject{}, fmt.Errorf("commit local object: %w", err)
	}
	committed = true
	checksum := hex.EncodeToString(hash.Sum(nil))
	contentType := input.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(input.Key))
	}
	return domain.StoredObject{
		ProviderAccountID: account.ID,
		RemoteBucket:      account.Bucket,
		RemoteKey:         remoteKey,
		Size:              written,
		ContentType:       contentType,
		ETag:              `"` + checksum + `"`,
		ChecksumSHA256:    checksum,
	}, nil
}

func (a *LocalAdapter) Quota(_ context.Context, account domain.ProviderAccount) (int64, int64, string, error) {
	root, err := a.providerRoot(account)
	if err != nil {
		return 0, 0, "local-filesystem", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, 0, "local-filesystem", err
	}
	var stats unix.Statfs_t
	if err := unix.Statfs(root, &stats); err != nil {
		return 0, 0, "local-filesystem", fmt.Errorf("stat local filesystem quota: %w", err)
	}
	blockSize := int64(stats.Bsize)
	used := int64(stats.Blocks-stats.Bfree) * blockSize
	available := int64(stats.Bavail) * blockSize
	return used + available, used, "local-filesystem", nil
}

func (a *LocalAdapter) Capabilities(domain.ProviderAccount) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{ListObjects: true, DiscoverBuckets: true, RemoteQuota: true, Checksums: true}
}

func (a *LocalAdapter) DiscoverBuckets(_ context.Context, account domain.ProviderAccount) ([]domain.ProviderBucket, error) {
	root, err := a.providerRoot(account)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []domain.ProviderBucket{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list local buckets: %w", err)
	}
	result := make([]domain.ProviderBucket, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			result = append(result, domain.ProviderBucket{Name: entry.Name()})
		}
	}
	return result, nil
}

func (a *LocalAdapter) ListObjects(_ context.Context, account domain.ProviderAccount, bucket, prefix, continuationToken string, limit int) (domain.ProviderObjectPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	root, err := a.objectPath(account, bucket, "inventory-probe")
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	root = filepath.Dir(root)
	objects := make([]domain.ProviderObject, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		key, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key = filepath.ToSlash(key)
		if key <= continuationToken || !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		objects = append(objects, domain.ProviderObject{Key: key, Size: info.Size(), ContentType: mime.TypeByExtension(filepath.Ext(key)), LastModified: info.ModTime().UTC()})
		if len(objects) > limit {
			return filepath.SkipAll
		}
		return nil
	})
	if os.IsNotExist(err) {
		return domain.ProviderObjectPage{Objects: []domain.ProviderObject{}}, nil
	}
	if err != nil {
		return domain.ProviderObjectPage{}, fmt.Errorf("inventory local objects: %w", err)
	}
	slices.SortFunc(objects, func(a, b domain.ProviderObject) int { return strings.Compare(a.Key, b.Key) })
	page := domain.ProviderObjectPage{Objects: objects}
	if len(page.Objects) > limit {
		page.Objects = page.Objects[:limit]
		page.NextContinuationToken = page.Objects[len(page.Objects)-1].Key
	}
	return page, nil
}

func (a *LocalAdapter) Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	path, err := a.objectPath(account, obj.Bucket, obj.RemoteKey)
	if err != nil {
		return nil, obj, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, obj, fmt.Errorf("open local object: %w", err)
	}
	return file, obj, nil
}

func (a *LocalAdapter) Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	path, err := a.objectPath(account, obj.Bucket, obj.RemoteKey)
	if err != nil {
		return obj, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return obj, fmt.Errorf("stat local object: %w", err)
	}
	obj.Size = info.Size()
	return obj, nil
}

func (a *LocalAdapter) Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error {
	path, err := a.objectPath(account, obj.Bucket, obj.RemoteKey)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete local object: %w", err)
	}
	return nil
}

func (a *LocalAdapter) Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	root, err := a.providerRoot(account)
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return unhealthy(account, started, "create local provider root: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return unhealthy(account, started, "stat local provider root: %v", err)
	}
	if !info.IsDir() {
		return unhealthy(account, started, "local provider root is not a directory")
	}
	probe := filepath.Join(root, ".bucketmux-healthcheck")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return unhealthy(account, started, "write local provider probe: %v", err)
	}
	_ = os.Remove(probe)
	return healthy(account, started, "local path is writable")
}

func (a *LocalAdapter) objectPath(account domain.ProviderAccount, bucket, key string) (string, error) {
	root, err := a.providerRoot(account)
	if err != nil {
		return "", err
	}
	safeBucket, err := safePathSegment(bucket)
	if err != nil {
		return "", fmt.Errorf("invalid local bucket path: %w", err)
	}
	safeKey, err := safeRelativePath(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, safeBucket, safeKey), nil
}

func (a *LocalAdapter) providerRoot(account domain.ProviderAccount) (string, error) {
	configured := ""
	if account.Settings != nil {
		configured = strings.TrimSpace(account.Settings["path"])
		if configured == "" {
			configured = strings.TrimSpace(account.Settings["root_path"])
		}
	}
	if configured == "" {
		accountID, err := safePathSegment(account.ID)
		if err != nil {
			return "", fmt.Errorf("invalid local provider id: %w", err)
		}
		return filepath.Join(a.baseDir, "objects", accountID), nil
	}
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured), nil
	}
	return filepath.Join(a.baseDir, filepath.Clean(configured)), nil
}

func safePathSegment(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return "", fmt.Errorf("%q is not a safe path segment", value)
	}
	return value, nil
}

func safeRelativePath(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), `/\`)
	if value == "" || value == "." {
		return "", fmt.Errorf("object key is required")
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' })
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("object key %q contains unsafe path segment", value)
		}
		clean = append(clean, part)
	}
	return filepath.Join(clean...), nil
}
