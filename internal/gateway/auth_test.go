package gateway

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
)

func TestAuthenticatorPresignedURL(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(5 * time.Minute) }

	signedURL, err := presignedURLForTest(http.MethodGet, "http://storage.local/images/cat.jpg", "test-access", "test-secret", "auto", signedAt, 900)
	if err != nil {
		t.Fatalf("presignedURLForTest() error = %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, signedURL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if !auth.Authorize(req) {
		t.Fatal("expected presigned URL to authorize")
	}
}

func TestAuthenticatorRejectsExpiredPresignedURL(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(16 * time.Minute) }

	signedURL, err := presignedURLForTest(http.MethodGet, "http://storage.local/images/cat.jpg", "test-access", "test-secret", "auto", signedAt, 900)
	if err != nil {
		t.Fatalf("presignedURLForTest() error = %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, signedURL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if auth.Authorize(req) {
		t.Fatal("expected expired presigned URL to be rejected")
	}
}

func TestAuthenticatorRejectsTamperedPresignedURL(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(5 * time.Minute) }

	signedURL, err := presignedURLForTest(http.MethodGet, "http://storage.local/images/cat.jpg", "test-access", "test-secret", "auto", signedAt, 900)
	if err != nil {
		t.Fatalf("presignedURLForTest() error = %v", err)
	}
	tampered := strings.Replace(signedURL, "/images/cat.jpg", "/images/dog.jpg", 1)
	req, err := http.NewRequest(http.MethodGet, tampered, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if auth.Authorize(req) {
		t.Fatal("expected tampered presigned URL to be rejected")
	}
}

func TestAuthenticatorHeaderSigV4(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(2 * time.Minute) }

	req, err := http.NewRequest(http.MethodPut, "http://storage.local/images/cat.jpg?x-id=PutObject", strings.NewReader("cat"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	signHeaderForTest(t, auth, req, signedAt, "UNSIGNED-PAYLOAD")
	if !auth.Authorize(req) {
		t.Fatal("expected header SigV4 request to authorize")
	}
}

func TestAuthenticatorRejectsTamperedHeaderSigV4(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(2 * time.Minute) }

	req, err := http.NewRequest(http.MethodGet, "http://storage.local/images/cat.jpg", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	signHeaderForTest(t, auth, req, signedAt, "UNSIGNED-PAYLOAD")
	req.URL.Path = "/images/dog.jpg"
	if auth.Authorize(req) {
		t.Fatal("expected tampered header SigV4 request to be rejected")
	}
}

func TestAuthenticatorRejectsStaleHeaderSigV4(t *testing.T) {
	signedAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	auth := NewAuthenticator(config.S3Config{AccessKey: "test-access", SecretKey: "test-secret", Region: "auto"})
	auth.now = func() time.Time { return signedAt.Add(16 * time.Minute) }

	req, err := http.NewRequest(http.MethodGet, "http://storage.local/images/cat.jpg", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	signHeaderForTest(t, auth, req, signedAt, "UNSIGNED-PAYLOAD")
	if auth.Authorize(req) {
		t.Fatal("expected stale header SigV4 request to be rejected")
	}
}

func signHeaderForTest(t *testing.T, auth *Authenticator, req *http.Request, signedAt time.Time, payloadHash string) {
	t.Helper()
	dateStamp := signedAt.UTC().Format("20060102")
	amzDate := signedAt.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest, ok := auth.headerCanonicalRequest(req, signedHeaders)
	if !ok {
		t.Fatal("could not build canonical request")
	}
	scope := dateStamp + "/auto/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonicalRequest)}, "\n")
	signature := hex.EncodeToString(hmacSHA256(deriveSigningKey("test-secret", dateStamp, "auto", "s3"), stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test-access/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func presignedURLForTest(method, rawURL, accessKey, secretKey, region string, signedAt time.Time, expires int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	dateStamp := signedAt.UTC().Format("20060102")
	amzDate := signedAt.UTC().Format("20060102T150405Z")
	query := u.Query()
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", fmt.Sprintf("%s/%s/%s/s3/aws4_request", accessKey, dateStamp, region))
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", strconv.Itoa(expires))
	query.Set("X-Amz-SignedHeaders", "host")
	u.RawQuery = query.Encode()
	req, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		return "", err
	}
	auth := &Authenticator{accessKey: accessKey, secretKey: secretKey, region: region, now: func() time.Time { return signedAt }}
	canonicalRequest, ok := auth.presignedCanonicalRequest(req, req.URL.Query(), "host")
	if !ok {
		return "", fmt.Errorf("could not create canonical request")
	}
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonicalRequest)}, "\n")
	signature := hex.EncodeToString(hmacSHA256(deriveSigningKey(secretKey, dateStamp, region, "s3"), stringToSign))
	query = u.Query()
	query.Set("X-Amz-Signature", signature)
	u.RawQuery = query.Encode()
	return u.String(), nil
}
