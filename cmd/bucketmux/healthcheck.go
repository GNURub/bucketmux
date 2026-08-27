package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultHealthcheckURL = "http://127.0.0.1:8080/readyz"

func healthcheck(ctx context.Context, rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		rawURL = defaultHealthcheckURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request health endpoint: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected health status: %s", resp.Status)
	}
	return nil
}

func configuredHealthcheckURL() string {
	if rawURL := strings.TrimSpace(os.Getenv("HEALTHCHECK_URL")); rawURL != "" {
		return rawURL
	}
	return defaultHealthcheckURL
}
