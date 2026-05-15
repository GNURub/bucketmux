package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestVercelBlobAdapterRoundTripAgainstAPIShape(t *testing.T) {
	objects := map[string][]byte{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer vercel_blob_rw_store123_secret" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPut && strings.TrimRight(r.URL.Path, "/") == "/api/blob":
			if got := r.Header.Get("X-Api-Version"); got != vercelBlobAPIVersion {
				t.Fatalf("X-Api-Version = %q", got)
			}
			if got := r.Header.Get("X-Vercel-Blob-Access"); got != "public" {
				t.Fatalf("X-Vercel-Blob-Access = %q", got)
			}
			if got := r.Header.Get("X-Allow-Overwrite"); got != "1" {
				t.Fatalf("X-Allow-Overwrite = %q", got)
			}
			if got := r.Header.Get("X-Content-Type"); got != "text/plain" {
				t.Fatalf("X-Content-Type = %q", got)
			}
			key := r.URL.Query().Get("pathname")
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
			return jsonResponse(http.StatusOK, map[string]string{
				"url":         "https://store123.public.blob.vercel-storage.com/" + key,
				"downloadUrl": "https://store123.public.blob.vercel-storage.com/" + key + "?download=1",
				"pathname":    key,
				"contentType": "text/plain",
				"etag":        "etag-1",
			}), nil
		case r.Method == http.MethodGet && strings.TrimRight(r.URL.Path, "/") == "/api/blob" && r.URL.Query().Has("url"):
			key := r.URL.Query().Get("url")
			data, ok := objects[key]
			if !ok {
				return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
			}
			return jsonResponse(http.StatusOK, map[string]any{"pathname": key, "size": len(data), "contentType": "text/plain", "etag": "etag-1", "uploadedAt": time.Now().UTC().Format(time.RFC3339)}), nil
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/blob/"):
			key, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/blob/"))
			data, ok := objects[key]
			if !ok {
				return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain"}, "ETag": {"etag-1"}, "Content-Length": {strconv.Itoa(len(data))}}, Body: io.NopCloser(strings.NewReader(string(data))), ContentLength: int64(len(data))}, nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/blob/delete":
			var payload struct {
				URLs []string `json:"urls"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode delete payload: %v", err)
			}
			for _, key := range payload.URLs {
				delete(objects, key)
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true}), nil
		case r.Method == http.MethodGet && strings.TrimRight(r.URL.Path, "/") == "/api/blob" && r.URL.Query().Get("limit") == "1":
			return jsonResponse(http.StatusOK, map[string]any{"blobs": []any{}, "hasMore": false}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
			return nil, nil
		}
	})

	adapter := NewVercelBlobAdapter()
	adapter.client = &http.Client{Transport: transport}
	account := domain.ProviderAccount{ID: "vercel-test", Kind: domain.ProviderKindVercelBlob, Endpoint: "https://api.test/api/blob", Bucket: "vercel", SecretKey: "vercel_blob_rw_store123_secret", Enabled: true, Settings: map[string]string{"access": "public", "blob_base_url": "https://blob.test/blob"}}
	stored, err := adapter.Put(context.Background(), account, domain.PutObjectInput{Bucket: "images", Key: "folder/cat.txt", Size: 3, ContentType: "text/plain"}, strings.NewReader("cat"))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if stored.RemoteKey != "folder/cat.txt" || stored.ETag != "etag-1" || stored.ContentType != "text/plain" {
		t.Fatalf("stored = %+v", stored)
	}

	obj := domain.ObjectRecord{Bucket: "images", Key: "folder/cat.txt", ProviderAccountID: account.ID, RemoteBucket: account.Bucket, RemoteKey: stored.RemoteKey, Size: stored.Size, ContentType: stored.ContentType, ETag: stored.ETag}
	head, err := adapter.Head(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if head.Size != 3 || head.ContentType != "text/plain" || head.ETag != "etag-1" {
		t.Fatalf("head = %+v", head)
	}
	body, got, err := adapter.Get(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "cat" || got.Size != 3 || got.ContentType != "text/plain" {
		t.Fatalf("get data=%q obj=%+v", data, got)
	}
	if health := adapter.Health(context.Background(), account); health.Status != domain.ProviderHealthHealthy {
		t.Fatalf("health = %+v", health)
	}
	if err := adapter.Delete(context.Background(), account, obj); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok := objects["folder/cat.txt"]; ok {
		t.Fatal("object still exists after delete")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, payload any) *http.Response {
	body, _ := json.Marshal(payload)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func TestVercelBlobPathRejectsUnsafePath(t *testing.T) {
	for _, key := range []string{"", "../secret", "folder//file.txt", "folder/../file.txt"} {
		if _, err := vercelBlobPath(key); err == nil {
			t.Fatalf("vercelBlobPath(%q) expected error", key)
		}
	}
}
