package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/gateway"
	"github.com/gnurub/bucketmux/internal/provider"
)

const (
	migrationAdminUser     = "migration-admin"
	migrationAdminPassword = "migration-password"
	migrationAccessKey     = "migration-access"
	migrationSecretKey     = "migration-secret"
)

func TestMigrationAPIAndUIAcrossS3LikeAndLocalProviders(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_MIGRATION_E2E") != "1" {
		t.Skip("set BUCKETMUX_RUN_MIGRATION_E2E=1 to run migration API/UI end-to-end tests")
	}
	playwrightCLI := strings.TrimSpace(os.Getenv("PLAYWRIGHT_CLI"))
	if playwrightCLI == "" {
		t.Fatal("PLAYWRIGHT_CLI must point to the pinned playwright-cli executable")
	}

	dataDir := t.TempDir()
	localDir := filepath.Join(dataDir, "local-provider")
	preexistingAPIObjects := map[string][]byte{
		"preexisting/api/root.jpg":                   {0xff, 0xd8, 0xff, 0xe0, 'a', 'p', 'i'},
		"preexisting/api/albums/summer.png":          {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 'a', 'p', 'i'},
		"preexisting/api/albums/2026/raw/photo.webp": {'R', 'I', 'F', 'F', 0x03, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'a', 'p', 'i'},
	}
	preexistingUIObjects := map[string][]byte{
		"preexisting/ui/cover.jpg":                 {0xff, 0xd8, 0xff, 0xe0, 'u', 'i'},
		"preexisting/ui/events/keynote.png":        {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 'u', 'i'},
		"preexisting/ui/events/day-2/gallery.webp": {'R', 'I', 'F', 'F', 0x02, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'u', 'i'},
	}
	writePreexistingLocalObjects(t, localDir, preexistingAPIObjects)
	writePreexistingLocalObjects(t, localDir, preexistingUIObjects)
	accounts := migrationProviderAccounts(localDir)
	svc, err := app.NewService(context.Background(), config.Config{
		Server: config.ServerConfig{
			Addr:      ":0",
			DataDir:   dataDir,
			DBPath:    filepath.Join(dataDir, "bucketmux.db"),
			MasterKey: "migration-e2e-master-key",
		},
		S3:      config.S3Config{AccessKey: migrationAccessKey, SecretKey: migrationSecretKey, Region: "us-east-1"},
		Admin:   config.AdminConfig{Enabled: true, Username: migrationAdminUser, Password: migrationAdminPassword},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{
			migrationProviderConfig(accounts["s3-source"]),
			migrationProviderConfig(accounts["s3-target"]),
			migrationProviderConfig(accounts["local-disk"]),
		},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	adminHandler := NewHandler(svc)
	mux := http.NewServeMux()
	mux.Handle("/admin", adminHandler)
	mux.Handle("/admin/", adminHandler)
	mux.Handle("/", gateway.NewHandler(svc))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cases := []migrationE2ECase{
		{name: "api s3-like to s3-like", trigger: "api", source: "s3-source", target: "s3-target", key: "api/s3-to-s3.txt", content: "API moved this object between independent S3-like providers"},
		{name: "api local to s3-like", trigger: "api", source: "local-disk", target: "s3-target", key: "api/local-to-s3.txt", content: "API moved this object from local disk to S3-like"},
		{name: "api s3-like to local", trigger: "api", source: "s3-source", target: "local-disk", key: "api/s3-to-local.txt", content: "API moved this object from S3-like to local disk"},
		{name: "ui s3-like to s3-like", trigger: "ui", source: "s3-source", target: "s3-target", key: "ui/s3-to-s3.txt", content: "UI moved this object between independent S3-like providers"},
		{name: "ui local to s3-like", trigger: "ui", source: "local-disk", target: "s3-target", key: "ui/local-to-s3.txt", content: "UI moved this object from local disk to S3-like"},
		{name: "ui s3-like to local", trigger: "ui", source: "s3-source", target: "local-disk", key: "ui/s3-to-local.txt", content: "UI moved this object from S3-like to local disk"},
	}

	originals := make(map[string]domain.ObjectRecord, len(cases))
	for _, testCase := range cases {
		setOnlyMigrationProviderEnabled(t, svc, testCase.source)
		originals[testCase.key] = putMigrationObjectThroughGateway(t, svc, server.URL, testCase)
	}
	setAllMigrationProvidersEnabled(t, svc)

	for _, testCase := range cases {
		if testCase.trigger != "api" {
			continue
		}
		t.Run(testCase.name, func(t *testing.T) {
			job := createMigrationThroughAPI(t, server.URL, testCase)
			waitForMigrationThroughAPI(t, server.URL, job.ID)
			requireMigrationMovedObject(t, svc, accounts, localDir, originals[testCase.key], testCase)
		})
	}

	uiCases := make([]migrationE2ECase, 0, 3)
	for _, testCase := range cases {
		if testCase.trigger == "ui" {
			uiCases = append(uiCases, testCase)
		}
	}
	runMigrationsThroughAdminUI(t, playwrightCLI, server.URL, uiCases)
	for _, testCase := range uiCases {
		t.Run(testCase.name, func(t *testing.T) {
			requireCompletedMigrationForPrefix(t, svc, testCase.key, 1)
			requireMigrationMovedObject(t, svc, accounts, localDir, originals[testCase.key], testCase)
		})
	}

	t.Run("pre-existing nested images through API inventory and move", func(t *testing.T) {
		inventory := createInventoryThroughAPI(t, server.URL, "preexisting/api/")
		waitForInventoryThroughAPI(t, server.URL, inventory.ID, len(preexistingAPIObjects))
		requireImportedLocalObjects(t, svc, localDir, preexistingAPIObjects)

		migrationCase := migrationE2ECase{source: "local-disk", target: "s3-target", key: "preexisting/api/", mode: domain.MigrationModeMove}
		job := createMigrationThroughAPI(t, server.URL, migrationCase)
		waitForMigrationThroughAPI(t, server.URL, job.ID, len(preexistingAPIObjects))
		requirePreexistingMoveToS3(t, svc, accounts, localDir, preexistingAPIObjects)
	})

	t.Run("pre-existing nested images through UI inventory and copy", func(t *testing.T) {
		runInventoryThroughAdminUI(t, playwrightCLI, server.URL, "preexisting/ui/", preexistingUIObjects)
		requireImportedLocalObjects(t, svc, localDir, preexistingUIObjects)

		migrationCase := migrationE2ECase{name: "UI copy of pre-existing nested images", trigger: "ui", source: "local-disk", target: "s3-target", key: "preexisting/ui/", mode: domain.MigrationModeCopy}
		runMigrationsThroughAdminUI(t, playwrightCLI, server.URL, []migrationE2ECase{migrationCase})
		requireCompletedMigrationForPrefix(t, svc, migrationCase.key, len(preexistingUIObjects))
		requirePreexistingCopyToS3(t, svc, accounts, localDir, preexistingUIObjects)
	})

	runAdvancedAdminControlsUI(t, playwrightCLI, server.URL, uiCases[2].key, uiCases[2].content)
	bucket, err := svc.Store.GetBucket(t.Context(), "images")
	if err != nil || !bucket.VersioningEnabled || !bucket.TrashEnabled || bucket.TrashRetentionDays != 30 {
		t.Fatalf("UI bucket protection=%+v err=%v", bucket, err)
	}
	credentials, err := svc.Store.ListAccessCredentials(t.Context())
	if err != nil || len(credentials) == 0 || credentials[0].Role != domain.AccessRoleReadOnly {
		t.Fatalf("UI credentials=%+v err=%v", credentials, err)
	}
}

type migrationE2ECase struct {
	name    string
	trigger string
	source  string
	target  string
	key     string
	content string
	mode    string
}

func migrationProviderAccounts(localDir string) map[string]domain.ProviderAccount {
	return map[string]domain.ProviderAccount{
		"s3-source": {
			ID: "s3-source", Name: "Independent S3-like source", Kind: domain.ProviderKindS3Compat,
			Endpoint: envOrMigrationDefault("MINISTACK_SOURCE_ENDPOINT", "http://127.0.0.1:4566"), Region: "us-east-1",
			Bucket: envOrMigrationDefault("MINISTACK_SOURCE_BUCKET", "bucketmux-migration-source"), AccessKey: "test", SecretKey: "test",
			CapacityBytes: 1024 * 1024 * 1024, Priority: 1, Enabled: true,
		},
		"s3-target": {
			ID: "s3-target", Name: "Independent S3-like target", Kind: domain.ProviderKindS3Compat,
			Endpoint: envOrMigrationDefault("MINISTACK_TARGET_ENDPOINT", "http://127.0.0.1:4567"), Region: "us-east-1",
			Bucket: envOrMigrationDefault("MINISTACK_TARGET_BUCKET", "bucketmux-migration-target"), AccessKey: "test", SecretKey: "test",
			CapacityBytes: 1024 * 1024 * 1024, Priority: 2, Enabled: true,
		},
		"local-disk": {
			ID: "local-disk", Name: "Local disk storage", Kind: domain.ProviderKindLocal, Bucket: "images",
			CapacityBytes: 1024 * 1024 * 1024, Priority: 3, Enabled: true, Settings: map[string]string{"path": localDir},
		},
	}
}

func migrationProviderConfig(account domain.ProviderAccount) config.ProviderConfig {
	enabled := account.Enabled
	return config.ProviderConfig{
		ID: account.ID, Name: account.Name, Kind: string(account.Kind), Endpoint: account.Endpoint, Region: account.Region,
		Bucket: account.Bucket, AccessKey: account.AccessKey, SecretKey: account.SecretKey, CapacityBytes: account.CapacityBytes,
		Priority: account.Priority, Enabled: &enabled, Settings: account.Settings,
	}
}

func setOnlyMigrationProviderEnabled(t *testing.T, svc *app.Service, providerID string) {
	t.Helper()
	setMigrationProvidersEnabled(t, svc, func(account domain.ProviderAccount) bool { return account.ID == providerID })
}

func setAllMigrationProvidersEnabled(t *testing.T, svc *app.Service) {
	t.Helper()
	setMigrationProvidersEnabled(t, svc, func(domain.ProviderAccount) bool { return true })
}

func setMigrationProvidersEnabled(t *testing.T, svc *app.Service, enabled func(domain.ProviderAccount) bool) {
	t.Helper()
	accounts, err := svc.Store.ListProviders(context.Background(), false)
	if err != nil {
		t.Fatalf("ListProviders() error = %v", err)
	}
	for _, account := range accounts {
		account.Enabled = enabled(account)
		if err := svc.Store.UpsertProvider(context.Background(), account); err != nil {
			t.Fatalf("UpsertProvider(%s) error = %v", account.ID, err)
		}
	}
}

func putMigrationObjectThroughGateway(t *testing.T, svc *app.Service, baseURL string, testCase migrationE2ECase) domain.ObjectRecord {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/images/"+testCase.key, strings.NewReader(testCase.content))
	if err != nil {
		t.Fatalf("NewRequest(PUT %s) error = %v", testCase.key, err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-S3LS-Access-Key", migrationAccessKey)
	req.Header.Set("X-S3LS-Secret-Key", migrationSecretKey)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("PUT %s error = %v", testCase.key, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("X-S3LS-Provider-Account") != testCase.source {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("PUT %s status=%d provider=%q body=%s", testCase.key, res.StatusCode, res.Header.Get("X-S3LS-Provider-Account"), body)
	}
	obj, err := svc.Store.GetObject(context.Background(), "images", testCase.key)
	if err != nil {
		t.Fatalf("GetObject(%s) after PUT error = %v", testCase.key, err)
	}
	return obj
}

func writePreexistingLocalObjects(t *testing.T, localDir string, objects map[string][]byte) {
	t.Helper()
	for key, content := range objects {
		path := filepath.Join(localDir, "images", filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
}

func createInventoryThroughAPI(t *testing.T, baseURL, prefix string) domain.InventoryJob {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"provider_account_id": "local-disk",
		"bucket":              "images",
		"remote_bucket":       "images",
		"prefix":              prefix,
		"mode":                domain.InventoryModeImport,
	})
	if err != nil {
		t.Fatalf("Marshal(inventory) error = %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/admin/api/inventory-jobs", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest(inventory) error = %v", err)
	}
	req.SetBasicAuth(migrationAdminUser, migrationAdminPassword)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST inventory error = %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST inventory status=%d body=%s", res.StatusCode, body)
	}
	var job domain.InventoryJob
	if err := json.NewDecoder(res.Body).Decode(&job); err != nil {
		t.Fatalf("Decode(inventory) error = %v", err)
	}
	return job
}

func waitForInventoryThroughAPI(t *testing.T, baseURL, jobID string, expectedObjects int) domain.InventoryJob {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/admin/api/inventory-jobs", nil)
		if err != nil {
			t.Fatalf("NewRequest(list inventory) error = %v", err)
		}
		req.SetBasicAuth(migrationAdminUser, migrationAdminPassword)
		res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET inventory error = %v", err)
		}
		var jobs []domain.InventoryJob
		decodeErr := json.NewDecoder(res.Body).Decode(&jobs)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || decodeErr != nil {
			t.Fatalf("GET inventory status=%d decode=%v", res.StatusCode, decodeErr)
		}
		for _, job := range jobs {
			if job.ID != jobID {
				continue
			}
			if job.Status == domain.InventoryStatusFailed {
				t.Fatalf("inventory %s failed: %s", job.ID, job.LastError)
			}
			if job.Status == domain.InventoryStatusCompleted {
				if job.DiscoveredObjects != expectedObjects || job.ImportedObjects != expectedObjects || job.MissingObjects != 0 {
					t.Fatalf("inventory counters = %+v", job)
				}
				return job
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("inventory %s did not complete before timeout", jobID)
	return domain.InventoryJob{}
}

func createMigrationThroughAPI(t *testing.T, baseURL string, testCase migrationE2ECase) domain.MigrationJob {
	t.Helper()
	mode := testCase.mode
	if mode == "" {
		mode = domain.MigrationModeMove
	}
	confirmation := ""
	if mode == domain.MigrationModeMove {
		confirmation = app.MigrationMoveConfirmationPhrase
	}
	payload, err := json.Marshal(map[string]string{
		"bucket": "images", "prefix": testCase.key, "source_provider_id": testCase.source, "target_provider_id": testCase.target,
		"mode": mode, "confirm": confirmation,
	})
	if err != nil {
		t.Fatalf("Marshal(migration) error = %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/admin/api/migrations", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest(migration) error = %v", err)
	}
	req.SetBasicAuth(migrationAdminUser, migrationAdminPassword)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST migration error = %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST migration status=%d body=%s", res.StatusCode, body)
	}
	var job domain.MigrationJob
	if err := json.NewDecoder(res.Body).Decode(&job); err != nil {
		t.Fatalf("Decode(migration) error = %v", err)
	}
	return job
}

func waitForMigrationThroughAPI(t *testing.T, baseURL, jobID string, expectedObjects ...int) domain.MigrationJob {
	t.Helper()
	wantObjects := 1
	if len(expectedObjects) > 0 {
		wantObjects = expectedObjects[0]
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/admin/api/migrations?limit=20", nil)
		if err != nil {
			t.Fatalf("NewRequest(list migrations) error = %v", err)
		}
		req.SetBasicAuth(migrationAdminUser, migrationAdminPassword)
		res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET migrations error = %v", err)
		}
		var jobs []domain.MigrationJob
		decodeErr := json.NewDecoder(res.Body).Decode(&jobs)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || decodeErr != nil {
			t.Fatalf("GET migrations status=%d decode=%v", res.StatusCode, decodeErr)
		}
		for _, job := range jobs {
			if job.ID != jobID {
				continue
			}
			if job.Status == domain.MigrationStatusFailed {
				t.Fatalf("migration %s failed: %s", job.ID, job.LastError)
			}
			if job.Status == domain.MigrationStatusCompleted {
				if job.TotalObjects != wantObjects || job.ProcessedObjects != wantObjects || job.SucceededObjects != wantObjects || job.FailedObjects != 0 {
					t.Fatalf("migration counters = %+v", job)
				}
				return job
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("migration %s did not complete before timeout", jobID)
	return domain.MigrationJob{}
}

func runInventoryThroughAdminUI(t *testing.T, cli, baseURL, prefix string, objects map[string][]byte) {
	t.Helper()
	session := fmt.Sprintf("bucketmux-inventory-ui-%d", time.Now().UnixNano())
	workDir := t.TempDir()
	t.Cleanup(func() {
		cmd := exec.Command(cli, "-s="+session, "close")
		cmd.Dir = workDir
		_, _ = cmd.CombinedOutput()
	})

	runMigrationPlaywrightCLI(t, cli, session, workDir, "open", "about:blank")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", fmt.Sprintf(`async page => { await page.context().setHTTPCredentials({ username: %q, password: %q }) }`, migrationAdminUser, migrationAdminPassword))
	runMigrationPlaywrightCLI(t, cli, session, workDir, "goto", baseURL+"/admin")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", `async page => { await page.waitForFunction(() => typeof window.htmx === 'object') }`)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")

	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	scenario, err := json.Marshal(map[string]any{"prefix": prefix, "expectedObjects": len(objects), "keys": keys})
	if err != nil {
		t.Fatalf("Marshal(UI inventory scenario) error = %v", err)
	}
	code := fmt.Sprintf(`async page => {
		const scenario = %s
		await page.locator('[data-open-dialog="inventory-dialog"]').first().click()
		await page.locator('#inventory-dialog').waitFor({ state: 'visible' })
		await page.locator('#inventory-provider').selectOption('local-disk')
		await page.locator('#inventory-test').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.toLowerCase().includes('healthy'))
		await page.locator('#inventory-discover').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.includes('Found 1 remote bucket'))
		await page.locator('#inventory-logical-bucket').selectOption('images')
		await page.locator('#inventory-bucket').fill('images')
		await page.locator('#inventory-prefix').fill(scenario.prefix)
		await page.locator('#inventory-mode').selectOption('import')
		await page.locator('#inventory-form button[type="submit"]').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.includes('started'))
		await page.waitForFunction(async ({ prefix, expectedObjects }) => {
			const jobs = await fetch('/admin/api/inventory-jobs').then((response) => response.json())
			const job = jobs.find((candidate) => candidate.prefix === prefix.replace(/^\/+/, ''))
			if (job?.status === 'failed') throw new Error('inventory failed: ' + job.last_error)
			return job?.status === 'completed' && job.discovered_objects === expectedObjects && job.imported_objects === expectedObjects
		}, scenario, { timeout: 45000 })
		await page.locator('#inventory-dialog [data-dialog-close]').first().click()

		await page.locator('#object-browser-bucket').selectOption('images')
		for (const key of scenario.keys) {
			const folder = key.slice(0, key.lastIndexOf('/') + 1)
			await page.locator('#object-browser-prefix').fill(folder)
			await page.locator('#object-browser-load').click()
			await page.waitForFunction((expectedKey) => {
				return [...document.querySelectorAll('#object-browser-rows tr')].some((row) => row.textContent.includes(expectedKey))
			}, key)
		}
	}`, scenario)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", code)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "console", "error")
}

func runMigrationsThroughAdminUI(t *testing.T, cli, baseURL string, cases []migrationE2ECase) {
	t.Helper()
	session := fmt.Sprintf("bucketmux-migration-ui-%d", time.Now().UnixNano())
	workDir := t.TempDir()
	t.Cleanup(func() {
		cmd := exec.Command(cli, "-s="+session, "close")
		cmd.Dir = workDir
		_, _ = cmd.CombinedOutput()
	})

	runMigrationPlaywrightCLI(t, cli, session, workDir, "open", "about:blank")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", fmt.Sprintf(`async page => { await page.context().setHTTPCredentials({ username: %q, password: %q }) }`, migrationAdminUser, migrationAdminPassword))
	runMigrationPlaywrightCLI(t, cli, session, workDir, "goto", baseURL+"/admin")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", `async page => { await page.waitForFunction(() => typeof window.htmx === 'object') }`)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", `async page => {
		await page.locator('[data-open-dialog="migration-dialog"]').first().click()
		await page.locator('#migration-dialog').waitFor({ state: 'visible' })
	}`)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")

	for _, testCase := range cases {
		mode := testCase.mode
		if mode == "" {
			mode = domain.MigrationModeMove
		}
		scenario, err := json.Marshal(map[string]string{"key": testCase.key, "source": testCase.source, "target": testCase.target, "mode": mode})
		if err != nil {
			t.Fatalf("Marshal(UI scenario) error = %v", err)
		}
		code := fmt.Sprintf(`async page => {
			const scenario = %s
			await page.locator('#migration-bucket').selectOption('images')
			await page.locator('#migration-prefix').fill(scenario.key)
			await page.locator('#migration-source-provider').selectOption(scenario.source)
			await page.locator('#migration-target-provider').selectOption(scenario.target)
			await page.locator('#migration-mode').selectOption(scenario.mode)
			await page.locator('#migration-confirm').fill(scenario.mode === 'move' ? 'Migrate permanently' : '')
			await page.locator('#migration-submit').click()
			await page.waitForFunction((key) => {
				const normalized = key.replace(/^\/+|\/+$/g, '')
				return [...document.querySelectorAll('#migration-job-rows tr')].some((row) => row.textContent.includes(normalized) && row.querySelector('.pill')?.textContent === 'completed')
			}, scenario.key, { timeout: 45000 })
		}`, scenario)
		runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", code)
		runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	}
}

func runAdvancedAdminControlsUI(t *testing.T, cli, baseURL, readableKey, readableContent string) {
	t.Helper()
	session := fmt.Sprintf("bucketmux-admin-controls-ui-%d", time.Now().UnixNano())
	workDir := t.TempDir()
	t.Cleanup(func() {
		cmd := exec.Command(cli, "-s="+session, "close")
		cmd.Dir = workDir
		_, _ = cmd.CombinedOutput()
	})
	runMigrationPlaywrightCLI(t, cli, session, workDir, "open", "about:blank")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", fmt.Sprintf(`async page => { await page.context().setHTTPCredentials({ username: %q, password: %q }) }`, migrationAdminUser, migrationAdminPassword))
	runMigrationPlaywrightCLI(t, cli, session, workDir, "goto", baseURL+"/admin")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	scenario, err := json.Marshal(map[string]string{"key": readableKey, "content": readableContent})
	if err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf(`async page => {
		const scenario = %s
		const providerRow = page.locator('#providers-card tbody tr').filter({ hasText: 'local-disk' })
		await providerRow.getByRole('button', { name: 'Test' }).click()
		await providerRow.getByRole('button', { name: 'Healthy' }).waitFor({ state: 'visible' })

		await page.locator('[data-open-dialog="inventory-dialog"]').first().click()
		await page.locator('#inventory-dialog').waitFor({ state: 'visible' })
		await page.locator('#inventory-provider').selectOption('local-disk')
		await page.locator('#inventory-test').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.toLowerCase().includes('healthy'))
		await page.locator('#inventory-discover').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.includes('Found 1 remote bucket'))
		await page.locator('#inventory-bucket').fill('images')
		await page.locator('#inventory-prefix').fill('ui/')
		await page.locator('#inventory-mode').selectOption('reconcile')
		await page.locator('#inventory-form button[type="submit"]').click()
		await page.waitForFunction(() => document.querySelector('#inventory-status')?.textContent.includes('started'))
		await page.locator('#inventory-dialog [data-dialog-close]').first().click()

		await page.locator('[data-open-dialog="credential-dialog"]').first().click()
		await page.locator('#credential-name').fill('E2E UI reader')
		await page.locator('#credential-role').selectOption('read-only')
		await page.locator('#credential-buckets').fill('images')
		await page.locator('#credential-prefixes').fill('ui/*')
		await page.locator('#credential-form button[type="submit"]').click()
		await page.waitForFunction(() => Boolean(document.querySelector('#credential-secret')?.value.includes('AWS_SECRET_ACCESS_KEY=')))
		const secretText = await page.locator('#credential-secret').inputValue()
		const credentials = Object.fromEntries(secretText.split('\n').map((line) => line.split('=')))
		const scopedRead = await page.evaluate(async ({ key, accessKey, secretKey }) => {
			const response = await fetch('/images/' + key, { headers: { 'X-S3LS-Access-Key': accessKey, 'X-S3LS-Secret-Key': secretKey } })
			return { status: response.status, body: await response.text() }
		}, { key: scenario.key, accessKey: credentials.AWS_ACCESS_KEY_ID, secretKey: credentials.AWS_SECRET_ACCESS_KEY })
		if (scopedRead.status !== 200 || scopedRead.body !== scenario.content) throw new Error('scoped credential read failed: ' + JSON.stringify(scopedRead))
		await page.locator('#credential-dialog [data-dialog-close]').first().click()

		await page.locator('[data-open-dialog="placement-dialog"]').click()
		await page.locator('#placement-bucket').fill('images')
		await page.locator('#placement-size').fill('4096')
		await page.locator('#placement-form button[type="submit"]').click()
		await page.waitForFunction(() => document.querySelector('#placement-status')?.textContent.includes('complete'))
		if (!(await page.locator('#placement-results').innerText()).includes('recommended')) throw new Error('placement simulation has no recommendation')
		await page.locator('#placement-dialog [data-dialog-close]').first().click()

		await page.locator('[data-open-dialog="repair-dialog"]').click()
		await page.locator('#repair-bucket').selectOption('images')
		await page.locator('#repair-prefix').fill('ui/')
		await page.locator('#repair-form button[type="submit"]').click()
		await page.waitForFunction(() => document.querySelector('#repair-status')?.textContent.includes('started'))
		await page.waitForFunction(async () => {
			const jobs = await fetch('/admin/api/repair-jobs').then((response) => response.json())
			return jobs.some((job) => job.prefix === 'ui/' && job.status === 'completed')
		}, null, { timeout: 15000 })
		await page.locator('#repair-dialog [data-dialog-close]').first().click()

		await page.locator('[data-open-dialog="bucket-dialog"]').first().click()
		const bucketForm = page.locator('#bucket-dialog form')
		await bucketForm.locator('input[name="name"]').fill('images')
		await bucketForm.locator('input[name="versioning_enabled"]').check()
		await bucketForm.locator('input[name="trash_enabled"]').check()
		await bucketForm.locator('input[name="trash_retention_days"]').fill('30')
		await Promise.all([page.waitForURL('**/admin'), bucketForm.locator('button[type="submit"]').click()])
		if (!(await page.locator('#buckets-card').innerText()).includes('versioned')) throw new Error('bucket protection did not render after save')
	}`, scenario)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "run-code", code)
	runMigrationPlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMigrationPlaywrightCLI(t, cli, session, workDir, "console", "error")
}

func runMigrationPlaywrightCLI(t *testing.T, cli, session, workDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(cli, append([]string{"-s=" + session}, args...)...)
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

func requireCompletedMigrationForPrefix(t *testing.T, svc *app.Service, prefix string, expectedObjects int) {
	t.Helper()
	jobs, err := svc.Store.ListMigrationJobs(context.Background(), 20)
	if err != nil {
		t.Fatalf("ListMigrationJobs() error = %v", err)
	}
	for _, job := range jobs {
		if job.Prefix == strings.Trim(prefix, "/") {
			if job.Status != domain.MigrationStatusCompleted || job.TotalObjects != expectedObjects || job.ProcessedObjects != expectedObjects || job.SucceededObjects != expectedObjects || job.FailedObjects != 0 {
				t.Fatalf("migration for %s = %+v", prefix, job)
			}
			return
		}
	}
	t.Fatalf("migration for prefix %s was not found", prefix)
}

func requireMigrationMovedObject(t *testing.T, svc *app.Service, accounts map[string]domain.ProviderAccount, localDir string, original domain.ObjectRecord, testCase migrationE2ECase) {
	t.Helper()
	obj, err := svc.Store.GetObject(context.Background(), "images", testCase.key)
	if err != nil {
		t.Fatalf("GetObject(%s) error = %v", testCase.key, err)
	}
	if obj.ProviderAccountID != testCase.target {
		t.Fatalf("%s provider = %s, want %s", testCase.key, obj.ProviderAccountID, testCase.target)
	}
	body, _, err := svc.GetObject(context.Background(), "images", testCase.key)
	if err != nil {
		t.Fatalf("GetObject content(%s) error = %v", testCase.key, err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || string(data) != testCase.content {
		t.Fatalf("%s content=%q readErr=%v, want %q", testCase.key, data, readErr, testCase.content)
	}

	requireMigrationProviderObjectState(t, accounts, localDir, testCase.source, original, false)
	requireMigrationProviderObjectState(t, accounts, localDir, testCase.target, obj, true)
}

func requireImportedLocalObjects(t *testing.T, svc *app.Service, localDir string, objects map[string][]byte) {
	t.Helper()
	for key, expected := range objects {
		obj, err := svc.Store.GetObject(context.Background(), "images", key)
		if err != nil {
			t.Fatalf("GetObject(%s) after inventory error = %v", key, err)
		}
		if obj.ProviderAccountID != "local-disk" || obj.RemoteBucket != "images" || obj.RemoteKey != key || obj.Size != int64(len(expected)) {
			t.Fatalf("imported object %s = %+v", key, obj)
		}
		localBytes, err := os.ReadFile(filepath.Join(localDir, "images", filepath.FromSlash(key)))
		if err != nil || !bytes.Equal(localBytes, expected) {
			t.Fatalf("local object %s bytes=%x err=%v, want %x", key, localBytes, err, expected)
		}
		requireGatewayObjectBytes(t, svc, key, expected)
	}
}

func requirePreexistingMoveToS3(t *testing.T, svc *app.Service, accounts map[string]domain.ProviderAccount, localDir string, objects map[string][]byte) {
	t.Helper()
	for key, expected := range objects {
		obj, err := svc.Store.GetObject(context.Background(), "images", key)
		if err != nil {
			t.Fatalf("GetObject(%s) after move error = %v", key, err)
		}
		if obj.ProviderAccountID != "s3-target" {
			t.Fatalf("%s provider = %s, want s3-target", key, obj.ProviderAccountID)
		}
		if _, err := os.Stat(filepath.Join(localDir, "images", filepath.FromSlash(key))); !os.IsNotExist(err) {
			t.Fatalf("moved local source %s still exists or returned unexpected error: %v", key, err)
		}
		requireGatewayObjectBytes(t, svc, key, expected)
		requireS3ObjectBytes(t, accounts["s3-target"], obj, expected)
	}
}

func requirePreexistingCopyToS3(t *testing.T, svc *app.Service, accounts map[string]domain.ProviderAccount, localDir string, objects map[string][]byte) {
	t.Helper()
	for key, expected := range objects {
		primary, err := svc.Store.GetObject(context.Background(), "images", key)
		if err != nil {
			t.Fatalf("GetObject(%s) after copy error = %v", key, err)
		}
		if primary.ProviderAccountID != "local-disk" {
			t.Fatalf("%s primary provider = %s, want local-disk", key, primary.ProviderAccountID)
		}
		localBytes, err := os.ReadFile(filepath.Join(localDir, "images", filepath.FromSlash(key)))
		if err != nil || !bytes.Equal(localBytes, expected) {
			t.Fatalf("copied local source %s bytes=%x err=%v, want %x", key, localBytes, err, expected)
		}
		replicas, err := svc.Store.ListObjectReplicas(context.Background(), "images", key)
		if err != nil {
			t.Fatalf("ListObjectReplicas(%s) error = %v", key, err)
		}
		var target *domain.ObjectReplica
		for index := range replicas {
			if replicas[index].ProviderAccountID == "s3-target" && replicas[index].Status == "succeeded" {
				target = &replicas[index]
				break
			}
		}
		if target == nil {
			t.Fatalf("%s replicas = %+v, want succeeded s3-target replica", key, replicas)
		}
		replicaObject := primary
		replicaObject.ProviderAccountID = target.ProviderAccountID
		replicaObject.RemoteBucket = target.RemoteBucket
		replicaObject.RemoteKey = target.RemoteKey
		replicaObject.Size = target.Size
		replicaObject.ETag = target.ETag
		replicaObject.ChecksumSHA256 = target.ChecksumSHA256
		requireGatewayObjectBytes(t, svc, key, expected)
		requireS3ObjectBytes(t, accounts["s3-target"], replicaObject, expected)
	}
}

func requireGatewayObjectBytes(t *testing.T, svc *app.Service, key string, expected []byte) {
	t.Helper()
	body, _, err := svc.GetObject(context.Background(), "images", key)
	if err != nil {
		t.Fatalf("GetObject content(%s) error = %v", key, err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(data, expected) {
		t.Fatalf("%s gateway bytes=%x readErr=%v, want %x", key, data, readErr, expected)
	}
}

func requireS3ObjectBytes(t *testing.T, account domain.ProviderAccount, obj domain.ObjectRecord, expected []byte) {
	t.Helper()
	body, _, err := provider.NewS3CompatAdapter().Get(context.Background(), account, obj)
	if err != nil {
		t.Fatalf("S3-like Get(%s) error = %v", obj.Key, err)
	}
	data, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(data, expected) {
		t.Fatalf("%s S3-like bytes=%x readErr=%v, want %x", obj.Key, data, readErr, expected)
	}
}

func requireMigrationProviderObjectState(t *testing.T, accounts map[string]domain.ProviderAccount, localDir, providerID string, obj domain.ObjectRecord, wantExists bool) {
	t.Helper()
	if providerID == "local-disk" {
		path := filepath.Join(localDir, "images", filepath.FromSlash(obj.RemoteKey))
		data, err := os.ReadFile(path)
		if wantExists && err != nil {
			t.Fatalf("local object %s should exist: %v", path, err)
		}
		if !wantExists && !os.IsNotExist(err) {
			t.Fatalf("local source object %s still exists or returned unexpected error: %v", path, err)
		}
		if wantExists && len(data) == 0 {
			t.Fatalf("local target object %s is empty", path)
		}
		return
	}

	adapter := provider.NewS3CompatAdapter()
	_, err := adapter.Head(context.Background(), accounts[providerID], obj)
	if wantExists && err != nil {
		t.Fatalf("S3-like target %s/%s should exist: %v", providerID, obj.Key, err)
	}
	if !wantExists && err == nil {
		t.Fatalf("S3-like source %s/%s still exists", providerID, obj.Key)
	}
	if !wantExists && !strings.Contains(err.Error(), "status=404") {
		t.Fatalf("S3-like source %s/%s returned %v, want a physical 404", providerID, obj.Key, err)
	}
}

func envOrMigrationDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
