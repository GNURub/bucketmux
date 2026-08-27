package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestUppyV6BrowserCompatibility(t *testing.T) {
	runBrowserCompatibilityTest(t, browserCompatibilityOptions{
		runEnv:        "BUCKETMUX_RUN_UPPY_BROWSER_E2E",
		bundle:        "fixture.js",
		page:          "index.html",
		providerID:    "ministack-uppy-e2e",
		defaultBucket: "bucketmux-uppy-e2e",
		sessionPrefix: "bucketmux-uppy",
	})
}

func TestFetchPresignedMultipartBrowserCompatibility(t *testing.T) {
	runBrowserCompatibilityTest(t, browserCompatibilityOptions{
		runEnv:        "BUCKETMUX_RUN_FETCH_BROWSER_E2E",
		bundle:        "fetch-fixture.js",
		page:          "fetch.html",
		providerID:    "ministack-fetch-e2e",
		defaultBucket: "bucketmux-fetch-e2e",
		sessionPrefix: "bucketmux-fetch",
	})
}

type browserCompatibilityOptions struct {
	runEnv        string
	bundle        string
	page          string
	providerID    string
	defaultBucket string
	sessionPrefix string
}

func runBrowserCompatibilityTest(t *testing.T, options browserCompatibilityOptions) {
	t.Helper()
	if os.Getenv(options.runEnv) != "1" {
		t.Skipf("set %s=1 to run this browser compatibility test", options.runEnv)
	}
	cli := strings.TrimSpace(os.Getenv("PLAYWRIGHT_CLI"))
	if cli == "" {
		t.Fatal("PLAYWRIGHT_CLI must point to the pinned playwright-cli executable")
	}
	fixtureDir := strings.TrimSpace(os.Getenv("BROWSER_FIXTURE_DIR"))
	if fixtureDir == "" {
		fixtureDir = strings.TrimSpace(os.Getenv("UPPY_FIXTURE_DIR"))
	}
	if fixtureDir == "" {
		t.Fatal("BROWSER_FIXTURE_DIR must point to the built browser fixture directory")
	}
	if _, err := os.Stat(filepath.Join(fixtureDir, "dist", options.bundle)); err != nil {
		t.Fatalf("browser fixture %s is not built: %v", options.bundle, err)
	}

	endpoint := envOrDefault("MINISTACK_ENDPOINT", "http://127.0.0.1:4566")
	remoteBucket := envOrDefault("MINISTACK_BUCKET", options.defaultBucket)
	dataDir := t.TempDir()
	gatewayConfig := config.S3Config{AccessKey: "browser-test-access", SecretKey: "browser-test-secret", Region: "us-east-1"}
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "bucketmux.db"), MasterKey: "browser-e2e-master-key"},
		S3:      gatewayConfig,
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            options.providerID,
			Name:          "MiniStack browser E2E",
			Kind:          string(domain.ProviderKindS3Compat),
			Endpoint:      endpoint,
			Region:        "us-east-1",
			Bucket:        remoteBucket,
			AccessKey:     "test",
			SecretKey:     "test",
			CapacityBytes: 1024 * 1024 * 1024,
			Priority:      1,
			Enabled:       new(true),
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	health, err := svc.ListProviderHealth(context.Background())
	if err != nil {
		t.Fatalf("ListProviderHealth() error = %v", err)
	}
	if len(health) != 1 || health[0].Status != domain.ProviderHealthHealthy {
		t.Fatalf("MiniStack provider health = %+v", health)
	}

	mux := http.NewServeMux()
	mux.Handle("/uppy/s3/", NewUppyHandler(svc))
	mux.Handle("/", NewHandler(svc))
	gatewayServer := httptest.NewServer(mux)
	t.Cleanup(gatewayServer.Close)
	fixtureServer := httptest.NewServer(http.FileServer(http.Dir(fixtureDir)))
	t.Cleanup(fixtureServer.Close)

	query := url.Values{
		"gateway":    {gatewayServer.URL},
		"accessKey":  {gatewayConfig.AccessKey},
		"secretKey":  {gatewayConfig.SecretKey},
		"bucket":     {"images"},
		"providerId": {options.providerID},
	}
	fixtureURL := fixtureServer.URL + "/" + options.page + "?" + query.Encode()
	session := fmt.Sprintf("%s-%d", options.sessionPrefix, time.Now().UnixNano())
	playwrightWorkDir := t.TempDir()
	t.Cleanup(func() {
		cmd := exec.Command(cli, "-s="+session, "close")
		cmd.Dir = playwrightWorkDir
		_, _ = cmd.CombinedOutput()
	})

	runPlaywrightCLI(t, cli, session, playwrightWorkDir, "open", fixtureURL)
	runPlaywrightCLI(t, cli, session, playwrightWorkDir, "snapshot")
	runPlaywrightCLI(t, cli, session, playwrightWorkDir, "run-code", `async page => {
		await page.waitForFunction(() => ["passed", "failed"].includes(document.body.dataset.status), null, { timeout: 120000 })
		const status = await page.locator("body").getAttribute("data-status")
		if (status !== "passed") throw new Error(await page.locator("#details").textContent() || "Uppy browser fixture failed")
	}`)
	runPlaywrightCLI(t, cli, session, playwrightWorkDir, "snapshot")
}

func runPlaywrightCLI(t *testing.T, cli, session, workDir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-s=" + session}, args...)
	cmd := exec.Command(cli, commandArgs...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CI=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		diagnostics := make([]string, 0, 3)
		for _, diagnostic := range [][]string{{"snapshot"}, {"console", "error"}, {"requests"}} {
			diagnosticCmd := exec.Command(cli, append([]string{"-s=" + session}, diagnostic...)...)
			diagnosticCmd.Dir = workDir
			if data, diagnosticErr := diagnosticCmd.CombinedOutput(); diagnosticErr == nil {
				diagnostics = append(diagnostics, string(data))
			}
		}
		t.Fatalf("playwright-cli %s failed: %v\n%s\n%s", strings.Join(args, " "), err, output, strings.Join(diagnostics, "\n"))
	}
	return string(output)
}
