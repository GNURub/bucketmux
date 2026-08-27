package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

type S3CompatAdapter struct {
	client *http.Client
}

func NewS3CompatAdapter() *S3CompatAdapter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = time.Second
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	return &S3CompatAdapter{client: &http.Client{Transport: transport}}
}

func (a *S3CompatAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	remoteKey := strings.TrimPrefix(input.StorageKey(), "/")
	endpoint, err := objectURL(account, account.Bucket, remoteKey)
	if err != nil {
		return domain.StoredObject{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return domain.StoredObject{}, err
	}
	if input.Size >= 0 {
		req.ContentLength = input.Size
	}
	if input.ContentType != "" {
		req.Header.Set("Content-Type", input.ContentType)
	}
	signRequest(req, account, unsignedPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("put s3-compatible object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.StoredObject{}, fmt.Errorf("put s3-compatible object failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		etag = `"streamed-upload"`
	}
	return domain.StoredObject{
		ProviderAccountID: account.ID,
		RemoteBucket:      account.Bucket,
		RemoteKey:         remoteKey,
		Size:              input.Size,
		ContentType:       input.ContentType,
		ETag:              etag,
	}, nil
}

func (a *S3CompatAdapter) Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	endpoint, err := objectURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return nil, obj, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, obj, err
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return nil, obj, fmt.Errorf("get s3-compatible object: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		return nil, obj, fmt.Errorf("get s3-compatible object failed: status=%d", res.StatusCode)
	}
	return res.Body, obj, nil
}

func (a *S3CompatAdapter) Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	endpoint, err := objectURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return obj, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return obj, err
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return obj, fmt.Errorf("head s3-compatible object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return obj, fmt.Errorf("head s3-compatible object failed: status=%d", res.StatusCode)
	}
	obj.ETag = res.Header.Get("ETag")
	obj.ContentType = res.Header.Get("Content-Type")
	obj.Size = res.ContentLength
	return obj, nil
}

func (a *S3CompatAdapter) Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error {
	endpoint, err := objectURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete s3-compatible object: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("delete s3-compatible object failed: status=%d", res.StatusCode)
	}
	return nil
}

func (a *S3CompatAdapter) Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	if account.Endpoint == "" {
		return unhealthy(account, started, "endpoint is required")
	}
	if account.Bucket == "" {
		return unhealthy(account, started, "bucket is required")
	}
	if account.AccessKey == "" || account.SecretKey == "" {
		return degraded(account, started, "access key or secret key is empty")
	}
	endpoint, err := bucketURL(account, account.Bucket)
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return unhealthy(account, started, "%s", err.Error())
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return unhealthy(account, started, "head bucket failed: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return healthy(account, started, "bucket responded to HeadBucket")
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized:
		return unhealthy(account, started, "credentials rejected: status=%d", res.StatusCode)
	case res.StatusCode == http.StatusNotFound:
		return unhealthy(account, started, "bucket not found: status=%d", res.StatusCode)
	default:
		return degraded(account, started, fmt.Sprintf("unexpected HeadBucket status=%d", res.StatusCode))
	}
}

type s3ListBucketsResult struct {
	Buckets struct {
		Bucket []struct {
			Name         string `xml:"Name"`
			CreationDate string `xml:"CreationDate"`
		} `xml:"Bucket"`
	} `xml:"Buckets"`
}

type s3ListObjectsResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	} `xml:"Contents"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

func (a *S3CompatAdapter) DiscoverBuckets(ctx context.Context, account domain.ProviderAccount) ([]domain.ProviderBucket, error) {
	endpoint, err := rootURL(account)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discover s3-compatible buckets: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("discover s3-compatible buckets failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded s3ListBucketsResult
	if err := xml.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode s3-compatible bucket list: %w", err)
	}
	result := make([]domain.ProviderBucket, 0, len(decoded.Buckets.Bucket))
	for _, bucket := range decoded.Buckets.Bucket {
		created, _ := time.Parse(time.RFC3339, bucket.CreationDate)
		result = append(result, domain.ProviderBucket{Name: bucket.Name, CreatedAt: created})
	}
	return result, nil
}

func (a *S3CompatAdapter) ListObjects(ctx context.Context, account domain.ProviderAccount, bucket, prefix, continuationToken string, limit int) (domain.ProviderObjectPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	endpoint, err := bucketURL(account, bucket)
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	query := parsed.Query()
	query.Set("list-type", "2")
	query.Set("max-keys", strconv.Itoa(limit))
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if continuationToken != "" {
		query.Set("continuation-token", continuationToken)
	}
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	signRequest(req, account, emptyPayloadHash)
	res, err := a.client.Do(req)
	if err != nil {
		return domain.ProviderObjectPage{}, fmt.Errorf("list s3-compatible objects: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.ProviderObjectPage{}, fmt.Errorf("list s3-compatible objects failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded s3ListObjectsResult
	if err := xml.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return domain.ProviderObjectPage{}, fmt.Errorf("decode s3-compatible object list: %w", err)
	}
	page := domain.ProviderObjectPage{NextContinuationToken: decoded.NextContinuationToken}
	for _, object := range decoded.Contents {
		modified, _ := time.Parse(time.RFC3339, object.LastModified)
		page.Objects = append(page.Objects, domain.ProviderObject{Key: object.Key, Size: object.Size, ETag: object.ETag, LastModified: modified})
	}
	return page, nil
}

func rootURL(account domain.ProviderAccount) (string, error) {
	if account.Endpoint == "" {
		return "", fmt.Errorf("provider %s endpoint is required", account.ID)
	}
	base, err := url.Parse(account.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("provider %s endpoint must be an absolute URL", account.ID)
	}
	return base.String(), nil
}

func bucketURL(account domain.ProviderAccount, bucket string) (string, error) {
	if account.Endpoint == "" {
		return "", fmt.Errorf("provider %s endpoint is required", account.ID)
	}
	base, err := url.Parse(account.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	base.Path = path.Join(base.Path, bucket)
	return base.String(), nil
}

func objectURL(account domain.ProviderAccount, bucket, key string) (string, error) {
	if account.Endpoint == "" {
		return "", fmt.Errorf("provider %s endpoint is required", account.ID)
	}
	base, err := url.Parse(account.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	base.Path = path.Join(base.Path, bucket, key)
	return base.String(), nil
}

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const unsignedPayloadHash = "UNSIGNED-PAYLOAD"

func signRequest(req *http.Request, account domain.ProviderAccount, payloadHash string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	region := account.Region
	if region == "" {
		region = "auto"
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalQuery := canonicalQuery(req.URL.Query())
	canonicalRequest := strings.Join([]string{
		req.Method,
		uriEncodePath(req.URL.EscapedPath()),
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256(canonicalRequest),
	}, "\n")
	signingKey := deriveSigningKey(account.SecretKey, dateStamp, region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", account.AccessKey, scope, signedHeaders, signature))
}

func canonicalHeaders(req *http.Request) (string, string) {
	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		var cleaned []string
		for _, value := range values {
			cleaned = append(cleaned, strings.Join(strings.Fields(value), " "))
		}
		headers[lower] = strings.Join(cleaned, ",")
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines strings.Builder
	for _, key := range keys {
		lines.WriteString(key)
		lines.WriteByte(':')
		lines.WriteString(headers[key])
		lines.WriteByte('\n')
	}
	return lines.String(), strings.Join(keys, ";")
}

func canonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0)
	for _, key := range keys {
		vals := append([]string(nil), values[key]...)
		sort.Strings(vals)
		for _, value := range vals {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncodePath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func hexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
