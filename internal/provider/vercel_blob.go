package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const (
	defaultVercelBlobAPIURL = "https://vercel.com/api/blob"
	vercelBlobAPIVersion    = "12"
	maxVercelBlobPathLength = 950
)

type VercelBlobAdapter struct {
	client *http.Client
}

func NewVercelBlobAdapter() *VercelBlobAdapter {
	return &VercelBlobAdapter{client: &http.Client{Timeout: 5 * time.Minute}}
}

func (a *VercelBlobAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	remoteKey, err := vercelBlobPath(input.StorageKey())
	if err != nil {
		return domain.StoredObject{}, err
	}
	endpoint, err := vercelBlobAPIURL(account, "/")
	if err != nil {
		return domain.StoredObject{}, err
	}
	endpoint.RawQuery = url.Values{"pathname": []string{remoteKey}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), body)
	if err != nil {
		return domain.StoredObject{}, err
	}
	if input.Size >= 0 {
		req.ContentLength = input.Size
		req.Header.Set("X-Content-Length", strconv.FormatInt(input.Size, 10))
	}
	setVercelBlobAuthHeaders(req, account)
	req.Header.Set("X-Vercel-Blob-Access", vercelBlobAccess(account))
	req.Header.Set("X-Add-Random-Suffix", "0")
	req.Header.Set("X-Allow-Overwrite", "1")
	if input.ContentType != "" {
		req.Header.Set("X-Content-Type", input.ContentType)
	}
	if cacheMaxAge := strings.TrimSpace(account.Settings["cache_control_max_age"]); cacheMaxAge != "" {
		req.Header.Set("X-Cache-Control-Max-Age", cacheMaxAge)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("put vercel blob object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.StoredObject{}, fmt.Errorf("put vercel blob object failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded vercelBlobPutResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return domain.StoredObject{}, fmt.Errorf("decode vercel blob put response: %w", err)
	}
	if decoded.Pathname != "" {
		remoteKey = decoded.Pathname
	}
	contentType := decoded.ContentType
	if contentType == "" {
		contentType = input.ContentType
	}
	etag := decoded.ETag
	if etag == "" {
		etag = `"vercel-blob"`
	}
	return domain.StoredObject{
		ProviderAccountID: account.ID,
		RemoteBucket:      account.Bucket,
		RemoteKey:         remoteKey,
		Size:              input.Size,
		ContentType:       contentType,
		ETag:              etag,
	}, nil
}

func (a *VercelBlobAdapter) Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	endpoint, err := vercelBlobObjectURL(account, obj.RemoteKey)
	if err != nil {
		return nil, obj, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, obj, err
	}
	setVercelBlobDownloadHeaders(req, account)
	res, err := a.client.Do(req)
	if err != nil {
		return nil, obj, fmt.Errorf("get vercel blob object: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		return nil, obj, fmt.Errorf("get vercel blob object failed: status=%d", res.StatusCode)
	}
	obj.ContentType = firstNonEmpty(res.Header.Get("Content-Type"), obj.ContentType)
	if res.ContentLength >= 0 {
		obj.Size = res.ContentLength
	}
	obj.ETag = firstNonEmpty(res.Header.Get("ETag"), obj.ETag)
	return res.Body, obj, nil
}

func (a *VercelBlobAdapter) Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	endpoint, err := vercelBlobAPIURL(account, "/")
	if err != nil {
		return obj, err
	}
	endpoint.RawQuery = url.Values{"url": []string{obj.RemoteKey}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return obj, err
	}
	setVercelBlobAuthHeaders(req, account)
	res, err := a.client.Do(req)
	if err != nil {
		return obj, fmt.Errorf("head vercel blob object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return obj, fmt.Errorf("head vercel blob object failed: status=%d", res.StatusCode)
	}
	var decoded vercelBlobHeadResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return obj, fmt.Errorf("decode vercel blob head response: %w", err)
	}
	if decoded.Size > 0 {
		obj.Size = decoded.Size
	}
	obj.ContentType = firstNonEmpty(decoded.ContentType, obj.ContentType)
	obj.ETag = firstNonEmpty(decoded.ETag, obj.ETag)
	return obj, nil
}

func (a *VercelBlobAdapter) Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error {
	endpoint, err := vercelBlobAPIURL(account, "/delete")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string][]string{"urls": {obj.RemoteKey}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	setVercelBlobAuthHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete vercel blob object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("delete vercel blob object failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (a *VercelBlobAdapter) Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	if vercelBlobToken(account) == "" {
		return degraded(account, started, "Vercel Blob read-write token is empty")
	}
	endpoint, err := vercelBlobAPIURL(account, "/")
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	endpoint.RawQuery = url.Values{"limit": []string{"1"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	setVercelBlobAuthHeaders(req, account)
	res, err := a.client.Do(req)
	if err != nil {
		return unhealthy(account, started, "Vercel Blob list probe failed: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return healthy(account, started, "Vercel Blob API responded")
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return unhealthy(account, started, "Vercel Blob token rejected: status=%d", res.StatusCode)
	default:
		return degraded(account, started, fmt.Sprintf("unexpected Vercel Blob API status=%d", res.StatusCode))
	}
}

type vercelBlobPutResponse struct {
	URL                string `json:"url"`
	DownloadURL        string `json:"downloadUrl"`
	Pathname           string `json:"pathname"`
	ContentType        string `json:"contentType"`
	ContentDisposition string `json:"contentDisposition"`
	ETag               string `json:"etag"`
}

type vercelBlobHeadResponse struct {
	URL                string `json:"url"`
	DownloadURL        string `json:"downloadUrl"`
	Pathname           string `json:"pathname"`
	Size               int64  `json:"size"`
	ContentType        string `json:"contentType"`
	ContentDisposition string `json:"contentDisposition"`
	CacheControl       string `json:"cacheControl"`
	UploadedAt         string `json:"uploadedAt"`
	ETag               string `json:"etag"`
}

func vercelBlobAPIURL(account domain.ProviderAccount, suffix string) (*url.URL, error) {
	base := strings.TrimSpace(account.Endpoint)
	if base == "" && account.Settings != nil {
		base = strings.TrimSpace(account.Settings["api_url"])
	}
	if base == "" {
		base = defaultVercelBlobAPIURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse vercel blob api url: %w", err)
	}
	if suffix != "" && suffix != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + suffix
	}
	return parsed, nil
}

func vercelBlobObjectURL(account domain.ProviderAccount, pathname string) (string, error) {
	if account.Settings != nil {
		if base := strings.TrimSpace(account.Settings["blob_base_url"]); base != "" {
			parsed, err := url.Parse(strings.TrimRight(base, "/") + "/" + escapeBlobPath(pathname))
			if err != nil {
				return "", fmt.Errorf("parse vercel blob base url: %w", err)
			}
			return parsed.String(), nil
		}
	}
	storeID := vercelBlobStoreID(account)
	if storeID == "" {
		return "", fmt.Errorf("vercel blob provider %s requires settings.store_id or a token containing the store id", account.ID)
	}
	return "https://" + storeID + "." + vercelBlobAccess(account) + ".blob.vercel-storage.com/" + escapeBlobPath(pathname), nil
}

func vercelBlobPath(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "/")
	if value == "" || len(value) > maxVercelBlobPathLength || strings.Contains(value, "//") {
		return "", fmt.Errorf("invalid Vercel Blob pathname %q", value)
	}
	parts := strings.SplitSeq(value, "/")
	for part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("vercel blob pathname %q contains unsafe segment", value)
		}
	}
	return value, nil
}

func escapeBlobPath(pathname string) string {
	parts := strings.Split(strings.Trim(pathname, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func vercelBlobAccess(account domain.ProviderAccount) string {
	if account.Settings != nil {
		access := strings.ToLower(strings.TrimSpace(account.Settings["access"]))
		if access == "private" {
			return "private"
		}
	}
	return "public"
}

func vercelBlobToken(account domain.ProviderAccount) string {
	if strings.TrimSpace(account.SecretKey) != "" {
		return strings.TrimSpace(account.SecretKey)
	}
	if strings.TrimSpace(account.AccessKey) != "" {
		return strings.TrimSpace(account.AccessKey)
	}
	return ""
}

func vercelBlobStoreID(account domain.ProviderAccount) string {
	if account.Settings != nil && strings.TrimSpace(account.Settings["store_id"]) != "" {
		return strings.TrimSpace(account.Settings["store_id"])
	}
	parts := strings.Split(vercelBlobToken(account), "_")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func setVercelBlobAuthHeaders(req *http.Request, account domain.ProviderAccount) {
	req.Header.Set("Authorization", "Bearer "+vercelBlobToken(account))
	req.Header.Set("X-Api-Version", vercelBlobAPIVersion)
}

func setVercelBlobDownloadHeaders(req *http.Request, account domain.ProviderAccount) {
	if token := vercelBlobToken(account); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
