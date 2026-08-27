package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

type CloudinaryAdapter struct {
	client *http.Client
}

func NewCloudinaryAdapter() *CloudinaryAdapter {
	return &CloudinaryAdapter{client: &http.Client{Timeout: 5 * time.Minute}}
}

func (a *CloudinaryAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	cloudName := cloudinaryCloudName(account)
	if cloudName == "" {
		return domain.StoredObject{}, fmt.Errorf("cloudinary provider %s requires settings.cloud_name", account.ID)
	}
	publicID := strings.Trim(strings.TrimPrefix(input.StorageKey(), "/"), "/")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	params := map[string]string{"public_id": publicID, "timestamp": timestamp, "overwrite": "true"}
	signature := signCloudinary(params, account.SecretKey)

	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		defer func() { _ = pipeWriter.Close() }()
		if err := writer.WriteField("api_key", account.AccessKey); err != nil {
			writeErr <- err
			return
		}
		if err := writer.WriteField("timestamp", timestamp); err != nil {
			writeErr <- err
			return
		}
		if err := writer.WriteField("public_id", publicID); err != nil {
			writeErr <- err
			return
		}
		if err := writer.WriteField("overwrite", "true"); err != nil {
			writeErr <- err
			return
		}
		if err := writer.WriteField("signature", signature); err != nil {
			writeErr <- err
			return
		}
		part, err := writer.CreateFormFile("file", publicID)
		if err != nil {
			writeErr <- err
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			_ = pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if err := writer.Close(); err != nil {
			writeErr <- err
			return
		}
	}()

	endpoint := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/image/upload", url.PathEscape(cloudName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeReader)
	if err != nil {
		return domain.StoredObject{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := a.client.Do(req)
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("upload cloudinary object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.StoredObject{}, fmt.Errorf("upload cloudinary object failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		PublicID  string `json:"public_id"`
		SecureURL string `json:"secure_url"`
		Bytes     int64  `json:"bytes"`
		Format    string `json:"format"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return domain.StoredObject{}, fmt.Errorf("decode cloudinary response: %w", err)
	}
	if err := <-writeErr; err != nil {
		return domain.StoredObject{}, fmt.Errorf("stream cloudinary object: %w", err)
	}
	return domain.StoredObject{
		ProviderAccountID: account.ID,
		RemoteBucket:      cloudName,
		RemoteKey:         decoded.PublicID,
		Size:              decoded.Bytes,
		ContentType:       input.ContentType,
		ETag:              decoded.SecureURL,
	}, nil
}

func (a *CloudinaryAdapter) Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	if obj.ETag == "" || !strings.HasPrefix(obj.ETag, "http") {
		return nil, obj, fmt.Errorf("cloudinary object has no delivery URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, obj.ETag, nil)
	if err != nil {
		return nil, obj, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, obj, err
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		return nil, obj, fmt.Errorf("get cloudinary object failed: status=%d", res.StatusCode)
	}
	return res.Body, obj, nil
}

func (a *CloudinaryAdapter) Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	if obj.ETag == "" || !strings.HasPrefix(obj.ETag, "http") {
		return obj, fmt.Errorf("cloudinary object has no delivery URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, obj.ETag, nil)
	if err != nil {
		return obj, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return obj, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return obj, fmt.Errorf("head cloudinary object failed: status=%d", res.StatusCode)
	}
	obj.Size = res.ContentLength
	obj.ContentType = res.Header.Get("Content-Type")
	return obj, nil
}

func (a *CloudinaryAdapter) Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error {
	cloudName := cloudinaryCloudName(account)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	params := map[string]string{"public_id": obj.RemoteKey, "timestamp": timestamp}
	form := url.Values{}
	form.Set("api_key", account.AccessKey)
	form.Set("timestamp", timestamp)
	form.Set("public_id", obj.RemoteKey)
	form.Set("signature", signCloudinary(params, account.SecretKey))
	endpoint := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/image/destroy", url.PathEscape(cloudName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("delete cloudinary object failed: status=%d", res.StatusCode)
	}
	return nil
}

func (a *CloudinaryAdapter) Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	cloudName := cloudinaryCloudName(account)
	if cloudName == "" {
		return unhealthy(account, started, "cloudinary cloud_name is required")
	}
	if account.AccessKey == "" || account.SecretKey == "" {
		return degraded(account, started, "api key or api secret is empty")
	}
	endpoint := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/resources/image?max_results=1", url.PathEscape(cloudName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	req.SetBasicAuth(account.AccessKey, account.SecretKey)
	res, err := a.client.Do(req)
	if err != nil {
		return unhealthy(account, started, "cloudinary api probe failed: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return healthy(account, started, "Cloudinary API responded")
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return unhealthy(account, started, "Cloudinary credentials rejected: status=%d", res.StatusCode)
	default:
		return degraded(account, started, fmt.Sprintf("unexpected Cloudinary API status=%d", res.StatusCode))
	}
}

func cloudinaryCloudName(account domain.ProviderAccount) string {
	if account.Settings != nil && account.Settings["cloud_name"] != "" {
		return account.Settings["cloud_name"]
	}
	return account.Bucket
}

func signCloudinary(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "&") + secret))
	return hex.EncodeToString(sum[:])
}
