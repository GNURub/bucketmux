package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func runAdminCLI(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bucketmux admin <providers|buckets|hooks|inventory|repairs|credentials|trash|cost|openapi|apply> [file]")
	}
	resourcePaths := map[string]string{
		"providers":   "/admin/api/providers",
		"buckets":     "/admin/api/buckets",
		"hooks":       "/admin/api/hooks",
		"inventory":   "/admin/api/inventory-jobs",
		"repairs":     "/admin/api/repair-jobs",
		"credentials": "/admin/api/access-credentials",
		"trash":       "/admin/api/trash",
		"cost":        "/admin/api/cost-optimizations",
		"openapi":     "/admin/openapi.json",
	}
	method := http.MethodGet
	path := resourcePaths[args[0]]
	var body []byte
	if args[0] == "apply" {
		if len(args) != 2 {
			return fmt.Errorf("usage: bucketmux admin apply <config.yaml>")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("read declarative config: %w", err)
		}
		var document any
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return fmt.Errorf("parse declarative config: %w", err)
		}
		body, err = json.Marshal(document)
		if err != nil {
			return fmt.Errorf("encode declarative config: %w", err)
		}
		method = http.MethodPost
		path = "/admin/api/declarative/apply"
	}
	if path == "" {
		return fmt.Errorf("unknown admin resource %q", args[0])
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BUCKETMUX_ADMIN_URL")), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.SetBasicAuth(os.Getenv("ADMIN_USER"), os.Getenv("ADMIN_PASSWORD"))
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("admin API request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("admin API returned %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	var decoded any
	if json.Unmarshal(responseBody, &decoded) == nil {
		pretty, _ := json.MarshalIndent(decoded, "", "  ")
		_, err = fmt.Fprintln(output, string(pretty))
		return err
	}
	_, err = output.Write(responseBody)
	return err
}
