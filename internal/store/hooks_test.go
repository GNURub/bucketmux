package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestStoreHooksCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hooks.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	hook := domain.Hook{
		ID:               "notify-store",
		Name:             "Notify store",
		Kind:             domain.HookKindHTTP,
		URL:              "https://example.com/hook",
		Method:           "POST",
		Events:           []string{domain.HookEventObjectCreated},
		HeadersEncrypted: "encrypted-headers",
		Enabled:          true,
	}
	if err := s.UpsertHook(ctx, hook); err != nil {
		t.Fatalf("UpsertHook() error = %v", err)
	}
	stored, err := s.GetHook(ctx, "notify-store")
	if err != nil {
		t.Fatalf("GetHook() error = %v", err)
	}
	if stored.ID != hook.ID || stored.URL != hook.URL || stored.HeadersEncrypted != "encrypted-headers" || len(stored.Events) != 1 || stored.Events[0] != domain.HookEventObjectCreated || !stored.Enabled {
		t.Fatalf("stored hook = %+v", stored)
	}

	stored.Enabled = false
	stored.Events = []string{domain.HookEventObjectDeleted}
	if err := s.UpsertHook(ctx, stored); err != nil {
		t.Fatalf("UpsertHook(update) error = %v", err)
	}
	enabled, err := s.ListHooks(ctx, true)
	if err != nil {
		t.Fatalf("ListHooks(enabled) error = %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("enabled hooks = %+v, want none", enabled)
	}
	all, err := s.ListHooks(ctx, false)
	if err != nil {
		t.Fatalf("ListHooks(all) error = %v", err)
	}
	if len(all) != 1 || all[0].Events[0] != domain.HookEventObjectDeleted {
		t.Fatalf("all hooks = %+v", all)
	}

	if err := s.DeleteHook(ctx, "notify-store"); err != nil {
		t.Fatalf("DeleteHook() error = %v", err)
	}
	if _, err := s.GetHook(ctx, "notify-store"); err != ErrNotFound {
		t.Fatalf("GetHook(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestStoreHookDeliveriesCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	hook := domain.Hook{ID: "notify-delivery", Name: "Notify delivery", Kind: domain.HookKindHTTP, URL: "https://example.com/hook", Method: "POST", Events: []string{domain.HookEventObjectCreated}, Enabled: true}
	if err := s.UpsertHook(ctx, hook); err != nil {
		t.Fatalf("UpsertHook() error = %v", err)
	}
	now := time.Now().UTC()
	delivery := domain.HookDelivery{
		ID:            "delivery-1",
		HookID:        hook.ID,
		Event:         domain.HookEventObjectCreated,
		Bucket:        "images",
		Key:           "demo.txt",
		PayloadJSON:   `{"event":"object.created"}`,
		Status:        domain.HookDeliveryStatusPending,
		MaxAttempts:   3,
		NextAttemptAt: now,
	}
	if err := s.CreateHookDelivery(ctx, delivery); err != nil {
		t.Fatalf("CreateHookDelivery() error = %v", err)
	}
	pending, err := s.ListPendingHookDeliveries(ctx, now.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("ListPendingHookDeliveries() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != delivery.ID {
		t.Fatalf("pending deliveries = %+v", pending)
	}
	stored, err := s.GetHookDelivery(ctx, delivery.ID)
	if err != nil {
		t.Fatalf("GetHookDelivery() error = %v", err)
	}
	stored.Status = domain.HookDeliveryStatusSucceeded
	stored.Attempts = 1
	stored.LastStatusCode = 204
	if err := s.UpdateHookDelivery(ctx, stored); err != nil {
		t.Fatalf("UpdateHookDelivery() error = %v", err)
	}
	all, err := s.ListHookDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListHookDeliveries() error = %v", err)
	}
	if len(all) != 1 || all[0].Status != domain.HookDeliveryStatusSucceeded || all[0].LastStatusCode != 204 {
		t.Fatalf("all deliveries = %+v", all)
	}
}
