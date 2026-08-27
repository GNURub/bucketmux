package gateway

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestMiniStackS3EndToEnd(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_MINISTACK_E2E") != "1" {
		t.Skip("set BUCKETMUX_RUN_MINISTACK_E2E=1 to run MiniStack end-to-end test")
	}
	endpoint := envOrDefault("MINISTACK_ENDPOINT", "http://127.0.0.1:4566")
	remoteBucket := envOrDefault("MINISTACK_BUCKET", "bucketmux-e2e")
	dataDir := t.TempDir()
	gatewayConfig := config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "us-east-1"}
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "bucketmux.db"), MasterKey: "ministack-e2e-master-key"},
		S3:      gatewayConfig,
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "ministack-e2e",
			Name:          "MiniStack E2E",
			Kind:          string(domain.ProviderKindS3Compat),
			Endpoint:      endpoint,
			Region:        "us-east-1",
			Bucket:        remoteBucket,
			AccessKey:     "test",
			SecretKey:     "test",
			CapacityBytes: 1024 * 1024 * 1024,
			Priority:      1,
			Enabled:       new(true),
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	health, err := svc.ListProviderHealth(context.Background())
	if err != nil {
		t.Fatalf("ListProviderHealth() error = %v", err)
	}
	if len(health) != 1 || health[0].Status != domain.ProviderHealthHealthy {
		t.Fatalf("MiniStack provider health = %+v", health)
	}

	server := newTestServer(NewHandler(svc))
	t.Cleanup(server.Close)
	auth := NewAuthenticator(gatewayConfig)
	prefix := fmt.Sprintf("e2e/%d", time.Now().UnixNano())
	simpleKey := prefix + "/simple.txt"
	copyKey := prefix + "/copied.txt"
	multipartKey := prefix + "/multipart.txt"

	put := doMiniStackGatewayRequest(t, auth, http.MethodPut, server.URL+"/images/"+simpleKey, "hello from ministack", map[string]string{"Content-Type": "text/plain", "X-Amz-Meta-Owner": "e2e", "X-Amz-Tagging": "environment=test&provider=ministack"})
	requireMiniStackStatus(t, put, http.StatusOK)
	if put.Header.Get("X-S3LS-Provider-Account") != "ministack-e2e" || put.Header.Get("ETag") == "" {
		t.Fatalf("PUT headers = %v", put.Header)
	}

	head := doMiniStackGatewayRequest(t, auth, http.MethodHead, server.URL+"/images/"+simpleKey, "", nil)
	requireMiniStackStatus(t, head, http.StatusOK)
	if head.Header.Get("Content-Length") != "20" || !strings.HasPrefix(head.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("HEAD headers = %v", head.Header)
	}

	get := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images/"+simpleKey, "", nil)
	requireMiniStackStatus(t, get, http.StatusOK)
	if get.Body != "hello from ministack" {
		t.Fatalf("GET body = %q", get.Body)
	}
	if get.Header.Get("X-Amz-Meta-Owner") != "e2e" {
		t.Fatalf("GET metadata headers = %v", get.Header)
	}
	tagging := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images/"+simpleKey+"?tagging", "", nil)
	requireMiniStackStatus(t, tagging, http.StatusOK)
	if !strings.Contains(tagging.Body, "environment") || !strings.Contains(tagging.Body, "ministack") {
		t.Fatalf("tagging body = %q", tagging.Body)
	}
	copyResponse := doMiniStackGatewayRequest(t, auth, http.MethodPut, server.URL+"/images/"+copyKey, "", map[string]string{"X-Amz-Copy-Source": "/images/" + simpleKey})
	requireMiniStackStatus(t, copyResponse, http.StatusOK)
	copied := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images/"+copyKey, "", nil)
	requireMiniStackStatus(t, copied, http.StatusOK)
	if copied.Body != "hello from ministack" || copied.Header.Get("X-Amz-Meta-Owner") != "e2e" {
		t.Fatalf("copied object = %+v", copied)
	}

	ranged := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images/"+simpleKey, "", map[string]string{"Range": "bytes=6-9"})
	requireMiniStackStatus(t, ranged, http.StatusPartialContent)
	if ranged.Body != "from" || ranged.Header.Get("Content-Range") != "bytes 6-9/20" {
		t.Fatalf("range response = %+v", ranged)
	}

	created := doMiniStackGatewayRequest(t, auth, http.MethodPost, server.URL+"/images/"+multipartKey+"?uploads", "", map[string]string{"Content-Type": "text/plain"})
	requireMiniStackStatus(t, created, http.StatusOK)
	var upload initiateMultipartUploadResult
	if err := xml.Unmarshal([]byte(created.Body), &upload); err != nil || upload.UploadID == "" {
		t.Fatalf("decode multipart creation: upload=%+v error=%v body=%q", upload, err, created.Body)
	}

	part1 := doMiniStackGatewayRequest(t, auth, http.MethodPut, server.URL+"/images/"+multipartKey+"?partNumber=1&uploadId="+upload.UploadID, "hello ", nil)
	requireMiniStackStatus(t, part1, http.StatusOK)
	part2 := doMiniStackGatewayRequest(t, auth, http.MethodPut, server.URL+"/images/"+multipartKey+"?partNumber=2&uploadId="+upload.UploadID, "from multipart", nil)
	requireMiniStackStatus(t, part2, http.StatusOK)
	completeBody := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", part1.Header.Get("ETag"), part2.Header.Get("ETag"))
	completed := doMiniStackGatewayRequest(t, auth, http.MethodPost, server.URL+"/images/"+multipartKey+"?uploadId="+upload.UploadID, completeBody, map[string]string{"Content-Type": "application/xml"})
	requireMiniStackStatus(t, completed, http.StatusOK)

	multipartGet := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images/"+multipartKey, "", nil)
	requireMiniStackStatus(t, multipartGet, http.StatusOK)
	if multipartGet.Body != "hello from multipart" {
		t.Fatalf("multipart GET body = %q", multipartGet.Body)
	}

	listed := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images?list-type=2&prefix="+prefix+"/", "", nil)
	requireMiniStackStatus(t, listed, http.StatusOK)
	var listing listBucketResult
	if err := xml.Unmarshal([]byte(listed.Body), &listing); err != nil {
		t.Fatalf("decode listing: %v body=%q", err, listed.Body)
	}
	found := map[string]bool{}
	for _, object := range listing.Contents {
		found[object.Key] = true
	}
	if !found[simpleKey] || !found[copyKey] || !found[multipartKey] {
		t.Fatalf("list contents = %+v", listing.Contents)
	}

	deleteXML := "<Delete><Object><Key>" + simpleKey + "</Key></Object><Object><Key>" + copyKey + "</Key></Object><Object><Key>" + multipartKey + "</Key></Object></Delete>"
	deleted := doMiniStackGatewayRequest(t, auth, http.MethodPost, server.URL+"/images?delete", deleteXML, map[string]string{"Content-Type": "application/xml"})
	requireMiniStackStatus(t, deleted, http.StatusOK)
	if strings.Count(deleted.Body, "<Deleted>") != 3 {
		t.Fatalf("multi-delete body = %q", deleted.Body)
	}
	empty := doMiniStackGatewayRequest(t, auth, http.MethodGet, server.URL+"/images?list-type=2&prefix="+prefix+"/", "", nil)
	requireMiniStackStatus(t, empty, http.StatusOK)
	listing = listBucketResult{}
	if err := xml.Unmarshal([]byte(empty.Body), &listing); err != nil || len(listing.Contents) != 0 {
		t.Fatalf("listing after delete = %+v error=%v", listing.Contents, err)
	}
}

type miniStackGatewayResponse struct {
	Status int
	Header http.Header
	Body   string
}

func doMiniStackGatewayRequest(t *testing.T, auth *Authenticator, method, rawURL, body string, headers map[string]string) miniStackGatewayResponse {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, rawURL, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	signHeaderForTest(t, auth, req, time.Now().UTC(), "UNSIGNED-PAYLOAD")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, rawURL, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, rawURL, err)
	}
	return miniStackGatewayResponse{Status: res.StatusCode, Header: res.Header.Clone(), Body: string(data)}
}

func requireMiniStackStatus(t *testing.T, response miniStackGatewayResponse, want int) {
	t.Helper()
	if response.Status != want {
		t.Fatalf("status = %d, want %d, body=%q", response.Status, want, response.Body)
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
