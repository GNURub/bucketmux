package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestScopedAccessCredentialEnforcementAndRotation(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	for key, content := range map[string]string{"public/read.txt": "allowed", "private/read.txt": "denied"} {
		request := httptest.NewRequest(http.MethodPut, "/images/"+key, strings.NewReader(content))
		addAuth(request)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("root PUT %s status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	created, err := handler.svc.CreateAccessCredential(t.Context(), app.AccessCredentialInput{Name: "Public reader", Role: domain.AccessRoleReadOnly, BucketPatterns: []string{"images"}, PrefixPatterns: []string{"public/*"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(request *http.Request, secret string) {
		request.Header.Set("X-S3LS-Access-Key", created.Credential.AccessKey)
		request.Header.Set("X-S3LS-Secret-Key", secret)
	}

	allowed := httptest.NewRequest(http.MethodGet, "/images/public/read.txt", nil)
	authorize(allowed, created.SecretKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, allowed)
	if response.Code != http.StatusOK || response.Body.String() != "allowed" {
		t.Fatalf("scoped GET status=%d body=%q", response.Code, response.Body.String())
	}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/images/private/read.txt", nil),
		httptest.NewRequest(http.MethodPut, "/images/public/write.txt", strings.NewReader("blocked")),
		httptest.NewRequest(http.MethodGet, "/other/public/read.txt", nil),
	} {
		authorize(request, created.SecretKey)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("out-of-scope %s %s status=%d", request.Method, request.URL, response.Code)
		}
	}

	rotated, err := handler.svc.RotateAccessCredential(t.Context(), created.Credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := httptest.NewRequest(http.MethodGet, "/images/public/read.txt", nil)
	authorize(oldSecret, created.SecretKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, oldSecret)
	if response.Code != http.StatusForbidden {
		t.Fatalf("old secret status=%d, want 403", response.Code)
	}
	newSecret := httptest.NewRequest(http.MethodGet, "/images/public/read.txt", nil)
	authorize(newSecret, rotated.SecretKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newSecret)
	if response.Code != http.StatusOK {
		t.Fatalf("rotated secret status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestExpiredAccessCredentialRejected(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	created, err := handler.svc.CreateAccessCredential(t.Context(), app.AccessCredentialInput{Name: "Expired", Role: domain.AccessRoleAdmin, Enabled: true, ExpiresAt: time.Now().UTC().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-S3LS-Access-Key", created.Credential.AccessKey)
	request.Header.Set("X-S3LS-Secret-Key", created.SecretKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expired credential status=%d body=%s", response.Code, response.Body.String())
	}
}
