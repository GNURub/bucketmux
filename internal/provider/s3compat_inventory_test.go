package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestS3CompatibleDiscoveryAndInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=access/") {
			http.Error(w, "missing signature", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<ListAllMyBucketsResult><Buckets><Bucket><Name>archive</Name><CreationDate>2026-01-01T00:00:00Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>`))
		case "/archive":
			if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "reports/" {
				http.Error(w, "invalid list query", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`<ListBucketResult><Contents><Key>reports/a.txt</Key><LastModified>2026-01-02T00:00:00Z</LastModified><ETag>"etag"</ETag><Size>42</Size></Contents><NextContinuationToken>next-token</NextContinuationToken></ListBucketResult>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := NewS3CompatAdapter()
	account := domain.ProviderAccount{ID: "remote", Kind: domain.ProviderKindS3Compat, Endpoint: server.URL, Region: "auto", Bucket: "archive", AccessKey: "access", SecretKey: "secret", Enabled: true}
	buckets, err := adapter.DiscoverBuckets(t.Context(), account)
	if err != nil || len(buckets) != 1 || buckets[0].Name != "archive" {
		t.Fatalf("buckets=%+v err=%v", buckets, err)
	}
	page, err := adapter.ListObjects(t.Context(), account, "archive", "reports/", "", 100)
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != "reports/a.txt" || page.Objects[0].Size != 42 || page.NextContinuationToken != "next-token" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
