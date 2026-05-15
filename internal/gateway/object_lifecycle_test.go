package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObjectLifecycleCompatibility(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()

	put := httptest.NewRequest(http.MethodPut, "/images/sdk/lifecycle.txt", strings.NewReader("hello lifecycle"))
	put.Header.Set("Content-Type", "text/plain")
	addAuth(put)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, put)
	if res.Code != http.StatusOK || res.Header().Get("ETag") == "" {
		t.Fatalf("put status = %d etag=%q body=%s", res.Code, res.Header().Get("ETag"), res.Body.String())
	}

	head := httptest.NewRequest(http.MethodHead, "/images/sdk/lifecycle.txt", nil)
	addAuth(head)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, head)
	if res.Code != http.StatusOK || res.Header().Get("Content-Length") != "15" || res.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("head status = %d headers=%v", res.Code, res.Header())
	}

	list := httptest.NewRequest(http.MethodGet, "/images?list-type=2&prefix=sdk/", nil)
	addAuth(list)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, list)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "<Key>sdk/lifecycle.txt</Key>") {
		t.Fatalf("list status = %d body=%s", res.Code, res.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/images/sdk/lifecycle.txt", nil)
	addAuth(get)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, get)
	if res.Code != http.StatusOK || res.Body.String() != "hello lifecycle" {
		t.Fatalf("get status = %d body=%q", res.Code, res.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/images/sdk/lifecycle.txt", nil)
	addAuth(deleteReq)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, deleteReq)
	if res.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", res.Code, res.Body.String())
	}

	head = httptest.NewRequest(http.MethodHead, "/images/sdk/lifecycle.txt", nil)
	addAuth(head)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, head)
	if res.Code != http.StatusNotFound {
		t.Fatalf("head after delete status = %d body=%s", res.Code, res.Body.String())
	}
}
