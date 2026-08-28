package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

const azureStorageVersion = "2023-11-03"

type AzureBlobAdapter struct {
	client *http.Client
}

func NewAzureBlobAdapter() *AzureBlobAdapter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 15 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = time.Second
	return &AzureBlobAdapter{client: &http.Client{Transport: transport}}
}

func (a *AzureBlobAdapter) Put(ctx context.Context, account domain.ProviderAccount, input domain.PutObjectInput, body io.Reader) (domain.StoredObject, error) {
	remoteKey := strings.TrimPrefix(input.StorageKey(), "/")
	endpoint, err := azureBlobURL(account, account.Bucket, remoteKey)
	if err != nil {
		return domain.StoredObject{}, err
	}
	hash := sha256.New()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, io.TeeReader(body, hash))
	if err != nil {
		return domain.StoredObject{}, err
	}
	req.ContentLength = input.Size
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	if input.ContentType != "" {
		req.Header.Set("Content-Type", input.ContentType)
	}
	if input.ChecksumSHA256 != "" {
		req.Header.Set("x-ms-meta-bucketmux-sha256", input.ChecksumSHA256)
	}
	res, err := a.do(req, account)
	if err != nil {
		return domain.StoredObject{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.StoredObject{}, HTTPError("put azure blob", res, string(responseBody))
	}
	checksum := input.ChecksumSHA256
	if checksum == "" {
		checksum = hex.EncodeToString(hash.Sum(nil))
	}
	return domain.StoredObject{ProviderAccountID: account.ID, RemoteBucket: account.Bucket, RemoteKey: remoteKey, Size: input.Size, ContentType: input.ContentType, ETag: res.Header.Get("ETag"), ChecksumSHA256: checksum}, nil
}

func (a *AzureBlobAdapter) Get(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (io.ReadCloser, domain.ObjectRecord, error) {
	endpoint, err := azureBlobURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return nil, obj, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, obj, err
	}
	res, err := a.do(req, account)
	if err != nil {
		return nil, obj, err
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, obj, HTTPError("get azure blob", res, string(responseBody))
	}
	obj.Size = res.ContentLength
	obj.ETag = res.Header.Get("ETag")
	obj.ContentType = res.Header.Get("Content-Type")
	obj.ChecksumSHA256 = res.Header.Get("x-ms-meta-bucketmux-sha256")
	return res.Body, obj, nil
}

func (a *AzureBlobAdapter) Head(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) (domain.ObjectRecord, error) {
	endpoint, err := azureBlobURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return obj, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return obj, err
	}
	res, err := a.do(req, account)
	if err != nil {
		return obj, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return obj, HTTPError("head azure blob", res, string(responseBody))
	}
	obj.Size = res.ContentLength
	obj.ETag = res.Header.Get("ETag")
	obj.ContentType = res.Header.Get("Content-Type")
	obj.ChecksumSHA256 = res.Header.Get("x-ms-meta-bucketmux-sha256")
	return obj, nil
}

func (a *AzureBlobAdapter) Delete(ctx context.Context, account domain.ProviderAccount, obj domain.ObjectRecord) error {
	endpoint, err := azureBlobURL(account, obj.RemoteBucket, obj.RemoteKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	res, err := a.do(req, account)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if (res.StatusCode < 200 || res.StatusCode > 299) && res.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return HTTPError("delete azure blob", res, string(responseBody))
	}
	return nil
}

func (a *AzureBlobAdapter) Health(ctx context.Context, account domain.ProviderAccount) domain.ProviderHealth {
	started := time.Now().UTC()
	if account.AccessKey == "" || account.SecretKey == "" {
		return degraded(account, started, "storage account name or account key is empty")
	}
	endpoint, err := azureContainerURL(account, account.Bucket)
	if err != nil {
		return unhealthy(account, started, "%s", err)
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("restype", "container")
	parsed.RawQuery = query.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, parsed.String(), nil)
	res, err := a.do(req, account)
	if err != nil {
		return unhealthy(account, started, "%s", err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return healthy(account, started, "Azure Blob container is reachable")
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return unhealthy(account, started, "credentials rejected: status=%d", res.StatusCode)
	case res.StatusCode == http.StatusNotFound:
		return unhealthy(account, started, "container not found")
	default:
		return degraded(account, started, fmt.Sprintf("unexpected container status=%d", res.StatusCode))
	}
}

func (a *AzureBlobAdapter) Capabilities(domain.ProviderAccount) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{ListObjects: true, DiscoverBuckets: true, Checksums: true, Limitations: []string{"Azure Blob does not expose an account storage quota through the data-plane API; capacity is configured and usage is reconciled from blob inventory."}}
}

type azureListContainersResult struct {
	Containers struct {
		Items []struct {
			Name       string `xml:"Name"`
			Properties struct {
				LastModified string `xml:"Last-Modified"`
			} `xml:"Properties"`
		} `xml:"Container"`
	} `xml:"Containers"`
}

func (a *AzureBlobAdapter) DiscoverBuckets(ctx context.Context, account domain.ProviderAccount) ([]domain.ProviderBucket, error) {
	root, err := azureRootURL(account)
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(root)
	query := parsed.Query()
	query.Set("comp", "list")
	parsed.RawQuery = query.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	res, err := a.do(req, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, HTTPError("list azure containers", res, string(responseBody))
	}
	var decoded azureListContainersResult
	if err := xml.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode azure container list: %w", err)
	}
	out := make([]domain.ProviderBucket, 0, len(decoded.Containers.Items))
	for _, item := range decoded.Containers.Items {
		created, _ := http.ParseTime(item.Properties.LastModified)
		out = append(out, domain.ProviderBucket{Name: item.Name, CreatedAt: created})
	}
	return out, nil
}

type azureListBlobsResult struct {
	Blobs struct {
		Items []struct {
			Name       string `xml:"Name"`
			Properties struct {
				LastModified  string `xml:"Last-Modified"`
				ContentLength int64  `xml:"Content-Length"`
				ContentType   string `xml:"Content-Type"`
				ETag          string `xml:"Etag"`
			} `xml:"Properties"`
		} `xml:"Blob"`
	} `xml:"Blobs"`
	NextMarker string `xml:"NextMarker"`
}

func (a *AzureBlobAdapter) ListObjects(ctx context.Context, account domain.ProviderAccount, bucket, prefix, continuationToken string, limit int) (domain.ProviderObjectPage, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	endpoint, err := azureContainerURL(account, bucket)
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("restype", "container")
	query.Set("comp", "list")
	query.Set("maxresults", strconv.Itoa(limit))
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if continuationToken != "" {
		query.Set("marker", continuationToken)
	}
	parsed.RawQuery = query.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	res, err := a.do(req, account)
	if err != nil {
		return domain.ProviderObjectPage{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return domain.ProviderObjectPage{}, HTTPError("list azure blobs", res, string(responseBody))
	}
	var decoded azureListBlobsResult
	if err := xml.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return domain.ProviderObjectPage{}, fmt.Errorf("decode azure blob list: %w", err)
	}
	page := domain.ProviderObjectPage{NextContinuationToken: decoded.NextMarker}
	for _, item := range decoded.Blobs.Items {
		modified, _ := http.ParseTime(item.Properties.LastModified)
		page.Objects = append(page.Objects, domain.ProviderObject{Key: item.Name, Size: item.Properties.ContentLength, ETag: item.Properties.ETag, ContentType: item.Properties.ContentType, LastModified: modified})
	}
	return page, nil
}

func (a *AzureBlobAdapter) do(req *http.Request, account domain.ProviderAccount) (*http.Response, error) {
	if err := signAzureRequest(req, account); err != nil {
		return nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, &Error{Op: strings.ToLower(req.Method) + " azure blob", Kind: FailureUnavailable, Err: err}
	}
	return res, nil
}

func azureRootURL(account domain.ProviderAccount) (string, error) {
	endpoint := strings.TrimSpace(account.Endpoint)
	if endpoint == "" && account.AccessKey != "" {
		endpoint = "https://" + account.AccessKey + ".blob.core.windows.net"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("provider %s Azure endpoint must be an absolute URL", account.ID)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func azureContainerURL(account domain.ProviderAccount, container string) (string, error) {
	root, err := azureRootURL(account)
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(root)
	parsed.Path = path.Join(parsed.Path, container)
	return parsed.String(), nil
}

func azureBlobURL(account domain.ProviderAccount, container, key string) (string, error) {
	root, err := azureContainerURL(account, container)
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(root)
	parsed.Path = path.Join(parsed.Path, strings.TrimPrefix(key, "/"))
	return parsed.String(), nil
}

func signAzureRequest(req *http.Request, account domain.ProviderAccount) error {
	if account.AccessKey == "" || account.SecretKey == "" {
		return &Error{Op: "sign azure request", Kind: FailureCredentials, Err: fmt.Errorf("storage account name and account key are required")}
	}
	key, err := base64.StdEncoding.DecodeString(account.SecretKey)
	if err != nil {
		return &Error{Op: "sign azure request", Kind: FailureCredentials, Err: fmt.Errorf("decode Azure account key: %w", err)}
	}
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureStorageVersion)
	contentLength := ""
	if req.ContentLength > 0 {
		contentLength = strconv.FormatInt(req.ContentLength, 10)
	}
	stringToSign := strings.Join([]string{
		req.Method,
		req.Header.Get("Content-Encoding"), req.Header.Get("Content-Language"), contentLength,
		req.Header.Get("Content-MD5"), req.Header.Get("Content-Type"), "",
		req.Header.Get("If-Modified-Since"), req.Header.Get("If-Match"), req.Header.Get("If-None-Match"),
		req.Header.Get("If-Unmodified-Since"), req.Header.Get("Range"),
	}, "\n") + "\n" + azureCanonicalHeaders(req.Header) + azureCanonicalResource(req.URL, account.AccessKey)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(stringToSign))
	req.Header.Set("Authorization", "SharedKey "+account.AccessKey+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return nil
}

func azureCanonicalHeaders(headers http.Header) string {
	values := map[string]string{}
	var names []string
	for name, entries := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-ms-") {
			continue
		}
		if _, exists := values[lower]; !exists {
			names = append(names, lower)
		}
		cleaned := make([]string, 0, len(entries))
		for _, entry := range entries {
			cleaned = append(cleaned, strings.Join(strings.Fields(entry), " "))
		}
		values[lower] = strings.Join(cleaned, ",")
	}
	sort.Strings(names)
	var result strings.Builder
	for _, name := range names {
		result.WriteString(name)
		result.WriteByte(':')
		result.WriteString(values[name])
		result.WriteByte('\n')
	}
	return result.String()
}

func azureCanonicalResource(resource *url.URL, accountName string) string {
	result := "/" + accountName + resource.EscapedPath()
	query := resource.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	for _, name := range names {
		values := append([]string(nil), query[name]...)
		sort.Strings(values)
		result += "\n" + name + ":" + strings.Join(values, ",")
	}
	return result
}
