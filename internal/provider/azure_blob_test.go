package provider

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

type azureRoundTripFunc func(*http.Request) (*http.Response, error)

func (function azureRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func azureTestAccount() domain.ProviderAccount {
	return domain.ProviderAccount{ID: "azure", Kind: domain.ProviderKindAzureBlob, Endpoint: "https://account.blob.core.windows.net", Bucket: "images", AccessKey: "account", SecretKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")), Enabled: true}
}

func TestAzureBlobPutUsesSharedKeyAndChecksumMetadata(t *testing.T) {
	var gotBody string
	adapter := &AzureBlobAdapter{client: &http.Client{Transport: azureRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(request.Body)
		gotBody = string(data)
		if !strings.HasPrefix(request.Header.Get("Authorization"), "SharedKey account:") {
			t.Fatalf("authorization=%q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("x-ms-blob-type") != "BlockBlob" || request.Header.Get("x-ms-version") != azureStorageVersion {
			t.Fatalf("headers=%v", request.Header)
		}
		if request.Header.Get("x-ms-meta-bucketmux-sha256") != "known-checksum" {
			t.Fatalf("checksum metadata=%q", request.Header.Get("x-ms-meta-bucketmux-sha256"))
		}
		response := &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}
		response.Header.Set("ETag", `"azure-etag"`)
		return response, nil
	})}}
	stored, err := adapter.Put(context.Background(), azureTestAccount(), domain.PutObjectInput{Bucket: "images", Key: "folder/demo.txt", Size: 5, ContentType: "text/plain", ChecksumSHA256: "known-checksum"}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != "hello" || stored.RemoteKey != "folder/demo.txt" || stored.ChecksumSHA256 != "known-checksum" || stored.ETag != `"azure-etag"` {
		t.Fatalf("body=%q stored=%+v", gotBody, stored)
	}
}

func TestAzureBlobInventoryAndErrorClassification(t *testing.T) {
	adapter := &AzureBlobAdapter{client: &http.Client{Transport: azureRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("comp") == "list" {
			body := `<EnumerationResults><Blobs><Blob><Name>folder/a.txt</Name><Properties><Last-Modified>Wed, 27 Aug 2026 10:00:00 GMT</Last-Modified><Etag>"etag"</Etag><Content-Length>7</Content-Length><Content-Type>text/plain</Content-Type></Properties></Blob></Blobs><NextMarker>next</NextMarker></EnumerationResults>`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`<Error><Code>AuthenticationFailed</Code></Error>`)), Request: request}, nil
	})}}
	page, err := adapter.ListObjects(context.Background(), azureTestAccount(), "images", "folder/", "", 100)
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Size != 7 || page.NextContinuationToken != "next" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	_, err = adapter.Put(context.Background(), azureTestAccount(), domain.PutObjectInput{Key: "denied", Size: 1}, strings.NewReader("x"))
	kind, _, ok := Failure(err)
	if !ok || kind != FailureCredentials {
		t.Fatalf("error=%v kind=%q", err, kind)
	}
}
