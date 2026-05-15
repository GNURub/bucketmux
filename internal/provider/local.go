package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

type LocalAdapter struct {
	baseDir string
}

func NewLocalAdapter(baseDir string) *LocalAdapter {
	return &LocalAdapter{baseDir: baseDir}
}

func (a *LocalAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	remoteKey, err := safeRelativePath(input.Key)
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
	file, err := os.Create(path)
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("create local object: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(file, io.TeeReader(body, hash))
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("write local object: %w", err)
	}
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
	value = strings.Trim(strings.TrimSpace(value), `/\\`)
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
