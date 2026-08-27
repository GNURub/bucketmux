package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if err := healthcheck(context.Background(), server.URL); err != nil {
		t.Fatalf("healthcheck failed: %v", err)
	}
}

func TestHealthcheckRejectsUnhealthyStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	if err := healthcheck(context.Background(), server.URL); err == nil {
		t.Fatal("expected unhealthy status to fail")
	}
}
