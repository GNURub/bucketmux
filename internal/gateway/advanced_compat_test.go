package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdvancedObjectCompatibility(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()

	put := httptest.NewRequest(http.MethodPut, "/images/source.txt", strings.NewReader("source-data"))
	put.Header.Set("Content-Type", "text/plain")
	put.Header.Set("X-Amz-Meta-Owner", "media-team")
	put.Header.Set("X-Amz-Tagging", "environment=test&kind=document")
	addAuth(put)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, put)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body.String())
	}
	etag := response.Header().Get("ETag")

	head := httptest.NewRequest(http.MethodHead, "/images/source.txt", nil)
	head.Header.Set("If-None-Match", etag)
	addAuth(head)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, head)
	if response.Code != http.StatusNotModified {
		t.Fatalf("conditional HEAD status=%d headers=%v", response.Code, response.Header())
	}

	tags := httptest.NewRequest(http.MethodGet, "/images/source.txt?tagging", nil)
	addAuth(tags)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, tags)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "environment") || !strings.Contains(response.Body.String(), "document") {
		t.Fatalf("GET tagging status=%d body=%s", response.Code, response.Body.String())
	}

	copyRequest := httptest.NewRequest(http.MethodPut, "/images/copied.txt", nil)
	copyRequest.Header.Set("X-Amz-Copy-Source", "/images/source.txt")
	addAuth(copyRequest)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, copyRequest)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "CopyObjectResult") {
		t.Fatalf("CopyObject status=%d body=%s", response.Code, response.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/images/copied.txt", nil)
	addAuth(get)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusOK || response.Body.String() != "source-data" || response.Header().Get("X-Amz-Meta-Owner") != "media-team" {
		t.Fatalf("copied GET status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
	}

	deleteBody := `<Delete><Object><Key>source.txt</Key></Object><Object><Key>copied.txt</Key></Object><Object><Key>missing.txt</Key></Object></Delete>`
	deleteRequest := httptest.NewRequest(http.MethodPost, "/images?delete", strings.NewReader(deleteBody))
	addAuth(deleteRequest)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, deleteRequest)
	if response.Code != http.StatusOK || strings.Count(response.Body.String(), "<Deleted>") != 3 {
		t.Fatalf("DeleteObjects status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestVersioningDeleteMarkerAndObjectLock(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()

	versioning := httptest.NewRequest(http.MethodPut, "/images?versioning", strings.NewReader(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
	addAuth(versioning)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, versioning)
	if response.Code != http.StatusOK {
		t.Fatalf("enable versioning status=%d body=%s", response.Code, response.Body.String())
	}

	putVersion := func(content string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPut, "/images/versioned.txt", strings.NewReader(content))
		addAuth(request)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != http.StatusOK || result.Header().Get("X-Amz-Version-Id") == "" {
			t.Fatalf("versioned PUT status=%d headers=%v body=%s", result.Code, result.Header(), result.Body.String())
		}
		return result.Header().Get("X-Amz-Version-Id")
	}
	versionOne := putVersion("one")
	versionTwo := putVersion("two")
	putOldTags := httptest.NewRequest(http.MethodPut, "/images/versioned.txt?tagging&versionId="+versionOne, strings.NewReader(`<Tagging><TagSet><Tag><Key>release</Key><Value>one</Value></Tag></TagSet></Tagging>`))
	addAuth(putOldTags)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, putOldTags)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT old version tags status=%d body=%s", response.Code, response.Body.String())
	}
	getOldTags := httptest.NewRequest(http.MethodGet, "/images/versioned.txt?tagging&versionId="+versionOne, nil)
	addAuth(getOldTags)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, getOldTags)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "release") || !strings.Contains(response.Body.String(), "one") {
		t.Fatalf("GET old version tags status=%d body=%s", response.Code, response.Body.String())
	}

	getOld := httptest.NewRequest(http.MethodGet, "/images/versioned.txt?versionId="+versionOne, nil)
	addAuth(getOld)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, getOld)
	if response.Code != http.StatusOK || response.Body.String() != "one" {
		t.Fatalf("GET old version status=%d body=%q", response.Code, response.Body.String())
	}

	deleteCurrent := httptest.NewRequest(http.MethodDelete, "/images/versioned.txt", nil)
	addAuth(deleteCurrent)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, deleteCurrent)
	marker := response.Header().Get("X-Amz-Version-Id")
	if response.Code != http.StatusNoContent || marker == "" || response.Header().Get("X-Amz-Delete-Marker") != "true" {
		t.Fatalf("versioned DELETE status=%d headers=%v", response.Code, response.Header())
	}

	removeMarker := httptest.NewRequest(http.MethodDelete, "/images/versioned.txt?versionId="+marker, nil)
	addAuth(removeMarker)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, removeMarker)
	if response.Code != http.StatusNoContent {
		t.Fatalf("remove delete marker status=%d body=%s", response.Code, response.Body.String())
	}
	getLatest := httptest.NewRequest(http.MethodGet, "/images/versioned.txt", nil)
	addAuth(getLatest)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, getLatest)
	if response.Code != http.StatusOK || response.Body.String() != "two" || response.Header().Get("X-Amz-Version-Id") != versionTwo {
		t.Fatalf("restored latest status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
	}

	lock := httptest.NewRequest(http.MethodPut, "/images?object-lock", strings.NewReader(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`))
	addAuth(lock)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, lock)
	if response.Code != http.StatusOK {
		t.Fatalf("enable object lock status=%d body=%s", response.Code, response.Body.String())
	}
	lockedPut := httptest.NewRequest(http.MethodPut, "/images/locked.txt", strings.NewReader("locked"))
	lockedPut.Header.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
	lockedPut.Header.Set("X-Amz-Object-Lock-Retain-Until-Date", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	addAuth(lockedPut)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, lockedPut)
	if response.Code != http.StatusOK {
		t.Fatalf("locked PUT status=%d body=%s", response.Code, response.Body.String())
	}
	lockedVersion := response.Header().Get("X-Amz-Version-Id")
	legalHold := httptest.NewRequest(http.MethodPut, "/images/locked.txt?legal-hold", strings.NewReader(`<LegalHold><Status>ON</Status></LegalHold>`))
	addAuth(legalHold)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, legalHold)
	if response.Code != http.StatusOK {
		t.Fatalf("set legal hold status=%d body=%s", response.Code, response.Body.String())
	}
	getVersionHold := httptest.NewRequest(http.MethodGet, "/images/locked.txt?legal-hold&versionId="+lockedVersion, nil)
	addAuth(getVersionHold)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, getVersionHold)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ON") {
		t.Fatalf("GET version legal hold status=%d body=%s", response.Code, response.Body.String())
	}
	legalHoldOff := httptest.NewRequest(http.MethodPut, "/images/locked.txt?legal-hold", strings.NewReader(`<LegalHold><Status>OFF</Status></LegalHold>`))
	addAuth(legalHoldOff)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, legalHoldOff)
	if response.Code != http.StatusOK {
		t.Fatalf("clear legal hold status=%d body=%s", response.Code, response.Body.String())
	}
	lockedDelete := httptest.NewRequest(http.MethodDelete, "/images/locked.txt", nil)
	addAuth(lockedDelete)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, lockedDelete)
	if response.Code != http.StatusForbidden {
		t.Fatalf("locked DELETE status=%d body=%s", response.Code, response.Body.String())
	}
	lockedDelete = httptest.NewRequest(http.MethodDelete, "/images/locked.txt", nil)
	lockedDelete.Header.Set("X-Amz-Bypass-Governance-Retention", "true")
	addAuth(lockedDelete)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, lockedDelete)
	if response.Code != http.StatusNoContent {
		t.Fatalf("governance bypass DELETE status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPresignedPOSTPolicyUpload(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	now := time.Now().UTC()
	credential := "ak/" + now.Format("20060102") + "/auto/s3/aws4_request"
	policyDocument := map[string]any{"expiration": now.Add(10 * time.Minute).Format(time.RFC3339), "conditions": []any{map[string]string{"bucket": "images"}, []any{"starts-with", "$key", "browser/"}, map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"}, map[string]string{"x-amz-credential": credential}, map[string]string{"x-amz-date": now.Format("20060102T150405Z")}, []any{"content-length-range", 1, 100}}}
	policyJSON, _ := json.Marshal(policyDocument)
	policy := base64.StdEncoding.EncodeToString(policyJSON)
	signature := hex.EncodeToString(hmacSHA256(deriveSigningKey("sk", now.Format("20060102"), "auto", "s3"), policy))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"key": "browser/${filename}", "policy": policy, "x-amz-algorithm": "AWS4-HMAC-SHA256", "x-amz-credential": credential, "x-amz-date": now.Format("20060102T150405Z"), "x-amz-signature": signature, "success_action_status": "201"}
	for name, value := range fields {
		_ = writer.WriteField(name, value)
	}
	part, _ := writer.CreateFormFile("file", "post.txt")
	_, _ = part.Write([]byte("presigned-post"))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/images", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "browser/post.txt") {
		t.Fatalf("presigned POST status=%d body=%s", response.Code, response.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/images/browser/post.txt", nil)
	addAuth(get)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusOK || response.Body.String() != "presigned-post" {
		t.Fatalf("GET POSTed object status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestTrashRestore(t *testing.T) {
	handler, cleanup := newGatewayTestHandler(t)
	defer cleanup()
	bucket, _ := handler.svc.Store.GetBucket(t.Context(), "images")
	bucket.TrashEnabled = true
	bucket.TrashRetentionDays = 7
	if err := handler.svc.Store.UpsertBucket(t.Context(), bucket); err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequest(http.MethodPut, "/images/trash.txt", strings.NewReader("recover-me"))
	addAuth(put)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, put)
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/images/trash.txt", nil)
	addAuth(deleteRequest)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, deleteRequest)
	trash, err := handler.svc.Store.ListTrashObjects(t.Context(), 10)
	if err != nil || len(trash) != 1 {
		t.Fatalf("trash=%+v err=%v", trash, err)
	}
	if _, err := handler.svc.RestoreTrashObject(t.Context(), trash[0].ID); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "/images/trash.txt", nil)
	addAuth(get)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusOK || response.Body.String() != "recover-me" {
		t.Fatalf("restored GET status=%d body=%q", response.Code, response.Body.String())
	}
}
