package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestPlainObjectCanOmitEmptyAttributeRecord(t *testing.T) {
	svc, cleanup := newHookTestService(t)
	defer cleanup()
	ctx := context.Background()
	input := domain.PutObjectInput{Bucket: "images", Key: "plain.txt", ContentType: "text/plain"}
	if _, err := svc.PutObject(ctx, input, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	plain, err := svc.GetProtectedObject(ctx, input.Bucket, input.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Metadata) != 0 || len(plain.Tags) != 0 || plain.VersionID != "" || plain.LegalHold || !plain.RetainUntil.IsZero() {
		t.Fatalf("unexpected default attributes: %+v", plain)
	}

	input.Metadata = map[string]string{"generation": "second"}
	input.Tags = map[string]string{"classified": "true"}
	if _, err := svc.PutObject(ctx, input, strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	withAttributes, err := svc.GetProtectedObject(ctx, input.Bucket, input.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if withAttributes.Metadata["generation"] != "second" || withAttributes.Tags["classified"] != "true" {
		t.Fatalf("attributes were not persisted: %+v", withAttributes)
	}

	input.Metadata = nil
	input.Tags = nil
	if _, err := svc.PutObject(ctx, input, strings.NewReader("third")); err != nil {
		t.Fatal(err)
	}
	cleared, err := svc.GetProtectedObject(ctx, input.Bucket, input.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Metadata) != 0 || len(cleared.Tags) != 0 {
		t.Fatalf("overwrite retained stale attributes: %+v", cleared)
	}
}

func TestPersistentObjectAttributeDetection(t *testing.T) {
	cases := []domain.ObjectRecord{
		{Metadata: map[string]string{"key": "value"}},
		{Tags: map[string]string{"key": "value"}},
		{VersionID: "v1"},
		{RetentionMode: "GOVERNANCE"},
		{RetainUntil: time.Now().Add(time.Hour)},
		{LegalHold: true},
	}
	if hasPersistentObjectAttributes(domain.ObjectRecord{}) {
		t.Fatal("empty object unexpectedly requires an attributes record")
	}
	for index, object := range cases {
		if !hasPersistentObjectAttributes(object) {
			t.Fatalf("case %d was not detected: %+v", index, object)
		}
	}
}
