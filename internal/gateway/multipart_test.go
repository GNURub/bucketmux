package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestMultipartUploadRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server: config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:     config.S3Config{AccessKey: "ak", SecretKey: "sk", Region: "auto"},
		Providers: []config.ProviderConfig{{
			ID:            "local-test",
			Name:          "Local test",
			Kind:          string(domain.ProviderKindLocal),
			Bucket:        "images",
			CapacityBytes: 1024 * 1024,
			Priority:      1,
			Enabled:       new(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
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

	handler := NewHandler(svc)
	req := httptest.NewRequest(http.MethodPost, "/images/big.txt?uploads", nil)
	addAuth(req)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", res.Code, res.Body.String())
	}
	var created initiateMultipartUploadResult
	if err := xml.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.UploadID == "" {
		t.Fatal("UploadID is empty")
	}

	uploadPart(t, handler, created.UploadID, 1, "hello")
	uploadPart(t, handler, created.UploadID, 2, "world")

	completeBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber></Part><Part><PartNumber>2</PartNumber></Part></CompleteMultipartUpload>`
	req = httptest.NewRequest(http.MethodPost, "/images/big.txt?uploadId="+created.UploadID, strings.NewReader(completeBody))
	addAuth(req)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("complete status = %d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/images/big.txt", nil)
	addAuth(req)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", res.Code, res.Body.String())
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read get body: %v", err)
	}
	if string(got) != "helloworld" {
		t.Fatalf("body = %q, want helloworld", got)
	}
}

func TestMultipartPartRejectsConfiguredSizeLimit(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	handler.svc.Config.Server.MaxMultipartPartBytes = 4

	create := httptest.NewRequest(http.MethodPost, "/images/limited.txt?uploads", nil)
	addAuth(create)
	createdResponse := httptest.NewRecorder()
	handler.ServeHTTP(createdResponse, create)
	var created initiateMultipartUploadResult
	if err := xml.NewDecoder(createdResponse.Body).Decode(&created); err != nil || created.UploadID == "" {
		t.Fatalf("create multipart response=%s error=%v", createdResponse.Body.String(), err)
	}

	part := httptest.NewRequest(http.MethodPut, "/images/limited.txt?partNumber=1&uploadId="+created.UploadID, strings.NewReader("12345"))
	addAuth(part)
	partResponse := httptest.NewRecorder()
	handler.ServeHTTP(partResponse, part)
	if partResponse.Code != http.StatusRequestEntityTooLarge || !strings.Contains(partResponse.Body.String(), "EntityTooLarge") {
		t.Fatalf("oversized multipart part status=%d body=%s", partResponse.Code, partResponse.Body.String())
	}
}

func TestOversizedMultipartRetryPreservesPreviousPart(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	handler.svc.Config.Server.MaxMultipartPartBytes = 4

	create := httptest.NewRequest(http.MethodPost, "/images/preserved.txt?uploads", nil)
	addAuth(create)
	createdResponse := httptest.NewRecorder()
	handler.ServeHTTP(createdResponse, create)
	var created initiateMultipartUploadResult
	if err := xml.NewDecoder(createdResponse.Body).Decode(&created); err != nil || created.UploadID == "" {
		t.Fatalf("create multipart response=%s error=%v", createdResponse.Body.String(), err)
	}

	validPart := httptest.NewRequest(http.MethodPut, "/images/preserved.txt?partNumber=1&uploadId="+created.UploadID, strings.NewReader("1234"))
	addAuth(validPart)
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, validPart)
	if validResponse.Code != http.StatusOK {
		t.Fatalf("valid part status=%d body=%s", validResponse.Code, validResponse.Body.String())
	}

	oversizedPart := httptest.NewRequest(http.MethodPut, "/images/preserved.txt?partNumber=1&uploadId="+created.UploadID, strings.NewReader("12345"))
	addAuth(oversizedPart)
	oversizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(oversizedResponse, oversizedPart)
	if oversizedResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized retry status=%d body=%s", oversizedResponse.Code, oversizedResponse.Body.String())
	}

	complete := httptest.NewRequest(http.MethodPost, "/images/preserved.txt?uploadId="+created.UploadID, strings.NewReader(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>`))
	addAuth(complete)
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/images/preserved.txt", nil)
	addAuth(get)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "1234" {
		t.Fatalf("preserved object status=%d body=%q", getResponse.Code, getResponse.Body.String())
	}
}

func uploadPart(t *testing.T, handler http.Handler, uploadID string, partNumber int, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/images/big.txt?partNumber="+strconv.Itoa(partNumber)+"&uploadId="+uploadID, bytes.NewBufferString(body))
	addAuth(req)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("upload part %d status = %d body=%s", partNumber, res.Code, res.Body.String())
	}
	if res.Header().Get("ETag") == "" {
		t.Fatalf("upload part %d missing ETag", partNumber)
	}
}

func addAuth(req *http.Request) {
	req.Header.Set("X-S3LS-Access-Key", "ak")
	req.Header.Set("X-S3LS-Secret-Key", "sk")
}
