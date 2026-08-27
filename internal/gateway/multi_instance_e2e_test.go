package gateway

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
)

const (
	multiInstanceAccessKey     = "multi-instance-access"
	multiInstanceSecretKey     = "multi-instance-secret"
	multiInstanceAdminUser     = "multi-admin"
	multiInstanceAdminPassword = "multi-password"
)

func TestMultiInstanceUserJourneys(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_MULTI_INSTANCE_E2E") != "1" {
		t.Skip("set BUCKETMUX_RUN_MULTI_INSTANCE_E2E=1 to run multi-instance end-to-end tests")
	}
	playwrightCLI := strings.TrimSpace(os.Getenv("PLAYWRIGHT_CLI"))
	if playwrightCLI == "" {
		t.Fatal("PLAYWRIGHT_CLI must point to the pinned playwright-cli executable")
	}

	instanceA := envOrDefault("MULTI_A_ENDPOINT", "http://127.0.0.1:18082")
	instanceB := envOrDefault("MULTI_B_ENDPOINT", "http://127.0.0.1:18083")
	proxy := envOrDefault("MULTI_PROXY_ENDPOINT", "http://127.0.0.1:18084")
	for name, endpoint := range map[string]string{"instance A": instanceA, "instance B": instanceB, "proxy": proxy} {
		requireMultiInstanceHealth(t, name, endpoint)
	}

	auth := NewAuthenticator(config.S3Config{
		AccessKey: multiInstanceAccessKey,
		SecretKey: multiInstanceSecretKey,
		Region:    "us-east-1",
	})
	prefix := fmt.Sprintf("multi-instance/%d", time.Now().UnixNano())
	simpleKey := prefix + "/cross-replica.txt"
	multipartKey := prefix + "/multipart.txt"
	uiKey := prefix + "/ui-upload.txt"

	t.Run("S3 object is immediately usable from another replica", func(t *testing.T) {
		content := "uploaded through instance A and read through instance B"
		put := doMultiInstanceGatewayRequest(t, http.MethodPut, instanceA+"/images/"+simpleKey, content, map[string]string{"Content-Type": "text/plain"})
		requireMiniStackStatus(t, put, http.StatusOK)
		if got := put.Header.Get("X-S3LS-Provider-Account"); got != "shared-ministack" {
			t.Fatalf("PUT provider = %q, want shared-ministack", got)
		}

		get := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/images/"+simpleKey, "", nil)
		requireMiniStackStatus(t, get, http.StatusOK)
		if get.Body != content {
			t.Fatalf("cross-replica GET body = %q, want %q", get.Body, content)
		}

		head := doMultiInstanceGatewayRequest(t, http.MethodHead, proxy+"/images/"+simpleKey, "", nil)
		requireMiniStackStatus(t, head, http.StatusOK)
		if head.Header.Get("Content-Length") != fmt.Sprint(len(content)) {
			t.Fatalf("HEAD content length = %q, want %d", head.Header.Get("Content-Length"), len(content))
		}

		ranged := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/images/"+simpleKey, "", map[string]string{"Range": "bytes=9-16"})
		requireMiniStackStatus(t, ranged, http.StatusPartialContent)
		if ranged.Body != "through " {
			t.Fatalf("range body = %q, want %q", ranged.Body, "through ")
		}

		listed := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/images?list-type=2&prefix="+url.QueryEscape(prefix+"/"), "", nil)
		requireMiniStackStatus(t, listed, http.StatusOK)
		if !strings.Contains(listed.Body, simpleKey) {
			t.Fatalf("cross-replica listing does not contain %q: %s", simpleKey, listed.Body)
		}

		target, err := url.Parse(proxy + "/images/" + simpleKey)
		if err != nil {
			t.Fatalf("parse presign target: %v", err)
		}
		presigned, ok := auth.PresignURL(http.MethodGet, *target, 5*time.Minute)
		if !ok {
			t.Fatal("could not create presigned GET URL")
		}
		presignedResponse := doUnsignedMultiInstanceRequest(t, http.MethodGet, presigned, "", nil)
		requireMiniStackStatus(t, presignedResponse, http.StatusOK)
		if presignedResponse.Body != content {
			t.Fatalf("presigned GET body = %q, want %q", presignedResponse.Body, content)
		}
	})

	t.Run("bucket metadata is shared", func(t *testing.T) {
		bucketName := fmt.Sprintf("team-assets-%d", time.Now().UnixNano())
		created := doMultiInstanceGatewayRequest(t, http.MethodPut, instanceA+"/"+bucketName, "", nil)
		requireMiniStackStatus(t, created, http.StatusOK)
		listed := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/", "", nil)
		requireMiniStackStatus(t, listed, http.StatusOK)
		if !strings.Contains(listed.Body, "<Name>"+bucketName+"</Name>") {
			t.Fatalf("bucket listing from instance B does not contain %q: %s", bucketName, listed.Body)
		}
	})

	t.Run("durable repair job is claimed once and visible across replicas", func(t *testing.T) {
		jobID := createMultiInstanceRepairJob(t, instanceA, "images", prefix+"/")
		job := waitMultiInstanceRepairJob(t, instanceB, jobID)
		if job.CheckedObjects < 1 || job.FailedObjects != 0 {
			t.Fatalf("repair job=%+v", job)
		}
	})

	t.Run("multipart remains sticky through the load balancer", func(t *testing.T) {
		created := doMultiInstanceGatewayRequest(t, http.MethodPost, proxy+"/images/"+multipartKey+"?uploads", "", map[string]string{"Content-Type": "text/plain"})
		requireMiniStackStatus(t, created, http.StatusOK)
		stickyUpstream := created.Header.Get("X-BucketMux-Upstream")
		if stickyUpstream == "" {
			t.Fatal("multipart creation did not expose the selected upstream")
		}
		var upload initiateMultipartUploadResult
		if err := xml.Unmarshal([]byte(created.Body), &upload); err != nil || upload.UploadID == "" {
			t.Fatalf("decode multipart creation: upload=%+v error=%v body=%q", upload, err, created.Body)
		}

		part1 := doMultiInstanceGatewayRequest(t, http.MethodPut, proxy+"/images/"+multipartKey+"?partNumber=1&uploadId="+url.QueryEscape(upload.UploadID), "hello from ", nil)
		requireMiniStackStatus(t, part1, http.StatusOK)
		requireStickyUpstream(t, part1, stickyUpstream)
		part2 := doMultiInstanceGatewayRequest(t, http.MethodPut, proxy+"/images/"+multipartKey+"?partNumber=2&uploadId="+url.QueryEscape(upload.UploadID), "sticky multipart", nil)
		requireMiniStackStatus(t, part2, http.StatusOK)
		requireStickyUpstream(t, part2, stickyUpstream)

		parts := doMultiInstanceGatewayRequest(t, http.MethodGet, proxy+"/images/"+multipartKey+"?uploadId="+url.QueryEscape(upload.UploadID), "", nil)
		requireMiniStackStatus(t, parts, http.StatusOK)
		requireStickyUpstream(t, parts, stickyUpstream)
		if !strings.Contains(parts.Body, "<PartNumber>1</PartNumber>") || !strings.Contains(parts.Body, "<PartNumber>2</PartNumber>") {
			t.Fatalf("multipart part listing is incomplete: %s", parts.Body)
		}

		completeBody := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", part1.Header.Get("ETag"), part2.Header.Get("ETag"))
		completed := doMultiInstanceGatewayRequest(t, http.MethodPost, proxy+"/images/"+multipartKey+"?uploadId="+url.QueryEscape(upload.UploadID), completeBody, map[string]string{"Content-Type": "application/xml"})
		requireMiniStackStatus(t, completed, http.StatusOK)
		requireStickyUpstream(t, completed, stickyUpstream)

		get := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/images/"+multipartKey, "", nil)
		requireMiniStackStatus(t, get, http.StatusOK)
		if get.Body != "hello from sticky multipart" {
			t.Fatalf("multipart content through instance B = %q", get.Body)
		}
	})

	t.Run("admin UI upload presign browse and delete", func(t *testing.T) {
		content := "uploaded by a user in the multi-instance admin UI"
		runMultiInstanceAdminJourney(t, playwrightCLI, proxy, uiKey, content)

		get := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceB+"/images/"+uiKey, "", nil)
		requireMiniStackStatus(t, get, http.StatusOK)
		if get.Body != content {
			t.Fatalf("UI upload read through instance B = %q, want %q", get.Body, content)
		}

		runMultiInstanceAdminDelete(t, playwrightCLI, proxy, uiKey)
		missing := doMultiInstanceGatewayRequest(t, http.MethodGet, instanceA+"/images/"+uiKey, "", nil)
		requireMiniStackStatus(t, missing, http.StatusNotFound)
	})

	for _, cleanup := range []struct {
		endpoint string
		key      string
	}{{instanceB, simpleKey}, {instanceA, multipartKey}} {
		deleted := doMultiInstanceGatewayRequest(t, http.MethodDelete, cleanup.endpoint+"/images/"+cleanup.key, "", nil)
		requireMiniStackStatus(t, deleted, http.StatusNoContent)
	}
}

func createMultiInstanceRepairJob(t *testing.T, endpoint, bucket, prefix string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"bucket": bucket, "prefix": prefix})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/admin/api/repair-jobs", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(multiInstanceAdminUser, multiInstanceAdminPassword)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var job struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil || response.StatusCode != http.StatusCreated || job.ID == "" {
		t.Fatalf("create repair status=%d job=%+v err=%v", response.StatusCode, job, err)
	}
	return job.ID
}

func waitMultiInstanceRepairJob(t *testing.T, endpoint, id string) struct {
	CheckedObjects int `json:"checked_objects"`
	FailedObjects  int `json:"failed_objects"`
} {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, endpoint+"/admin/api/repair-jobs", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth(multiInstanceAdminUser, multiInstanceAdminPassword)
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []struct {
			ID             string `json:"id"`
			Status         string `json:"status"`
			CheckedObjects int    `json:"checked_objects"`
			FailedObjects  int    `json:"failed_objects"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&jobs)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil {
			t.Fatalf("list repair status=%d err=%v", response.StatusCode, decodeErr)
		}
		for _, job := range jobs {
			if job.ID != id {
				continue
			}
			if job.Status == "failed" {
				t.Fatalf("repair job failed: %+v", job)
			}
			if job.Status == "completed" {
				return struct {
					CheckedObjects int `json:"checked_objects"`
					FailedObjects  int `json:"failed_objects"`
				}{CheckedObjects: job.CheckedObjects, FailedObjects: job.FailedObjects}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("repair job %s did not complete", id)
	return struct {
		CheckedObjects int `json:"checked_objects"`
		FailedObjects  int `json:"failed_objects"`
	}{}
}

func requireMultiInstanceHealth(t *testing.T, name, endpoint string) {
	t.Helper()
	response := doUnsignedMultiInstanceRequest(t, http.MethodGet, endpoint+"/healthz", "", nil)
	if response.Status != http.StatusOK {
		t.Fatalf("%s health status = %d, body=%q", name, response.Status, response.Body)
	}
}

func doMultiInstanceGatewayRequest(t *testing.T, method, rawURL, body string, headers map[string]string) miniStackGatewayResponse {
	t.Helper()
	authenticatedHeaders := make(map[string]string, len(headers)+2)
	for name, value := range headers {
		authenticatedHeaders[name] = value
	}
	authenticatedHeaders["X-S3LS-Access-Key"] = multiInstanceAccessKey
	authenticatedHeaders["X-S3LS-Secret-Key"] = multiInstanceSecretKey
	return doUnsignedMultiInstanceRequest(t, method, rawURL, body, authenticatedHeaders)
}

func doUnsignedMultiInstanceRequest(t *testing.T, method, rawURL, body string, headers map[string]string) miniStackGatewayResponse {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, rawURL, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, rawURL, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, rawURL, err)
	}
	return miniStackGatewayResponse{Status: res.StatusCode, Header: res.Header.Clone(), Body: string(data)}
}

func requireStickyUpstream(t *testing.T, response miniStackGatewayResponse, want string) {
	t.Helper()
	if got := response.Header.Get("X-BucketMux-Upstream"); got != want {
		t.Fatalf("multipart upstream = %q, want sticky upstream %q", got, want)
	}
}

func runMultiInstanceAdminJourney(t *testing.T, cli, baseURL, key, content string) {
	t.Helper()
	session := fmt.Sprintf("bucketmux-multi-instance-ui-%d", time.Now().UnixNano())
	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "ui-upload.txt")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write UI upload fixture: %v", err)
	}
	t.Setenv("BUCKETMUX_MULTI_INSTANCE_UI_SESSION", session)
	t.Cleanup(func() {
		cmd := exec.Command(cli, "-s="+session, "close")
		cmd.Dir = workDir
		_, _ = cmd.CombinedOutput()
	})

	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "open", "about:blank")
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "snapshot")
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "run-code", fmt.Sprintf(`async page => { await page.context().setHTTPCredentials({ username: %q, password: %q }) }`, multiInstanceAdminUser, multiInstanceAdminPassword))
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "goto", baseURL+"/admin")
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "snapshot")

	scenario, err := json.Marshal(map[string]string{"key": key, "content": content, "filePath": filePath})
	if err != nil {
		t.Fatalf("marshal admin UI scenario: %v", err)
	}
	code := fmt.Sprintf(`async page => {
		const scenario = %s
		if (!(await page.locator('body').innerText()).includes('Shared MiniStack provider')) throw new Error('shared provider is not visible')
		await page.locator('[data-open-dialog="upload-dialog"]').first().click()
		await page.locator('#upload-dialog').waitFor({ state: 'visible' })
		const form = page.locator('#admin-upload-form')
		await form.locator('select[name="bucket"]').selectOption('images')
		await form.locator('input[name="key"]').fill(scenario.key)
		await form.locator('input[name="file"]').setInputFiles(scenario.filePath)
		await form.locator('button[type="submit"]').click()
		await page.waitForFunction((key) => document.querySelector('#upload-status')?.textContent.includes('Uploaded images/' + key), scenario.key)
		await page.locator('#upload-dialog [data-dialog-close]').first().click()
		await page.locator('#object-browser-bucket').selectOption('images')
		await page.locator('#object-browser-prefix').fill(scenario.key)
		await page.locator('#object-browser-load').click()
		const row = page.locator('#object-browser-rows tr').filter({ hasText: scenario.key })
		await row.waitFor({ state: 'visible' })
		await row.getByRole('button', { name: 'Public URL' }).click()
		await page.waitForFunction(() => Boolean(document.querySelector('#object-public-url')?.value))
		const publicURL = await page.locator('#object-public-url').inputValue()
		const result = await page.evaluate(async ({ publicURL, content }) => {
			const response = await fetch(publicURL)
			return { status: response.status, body: await response.text() }
		}, { publicURL, content: scenario.content })
		if (result.status !== 200 || result.body !== scenario.content) throw new Error('presigned URL returned ' + JSON.stringify(result))
	}`, scenario)
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "run-code", code)
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "snapshot")
}

func runMultiInstanceAdminDelete(t *testing.T, cli, baseURL, key string) {
	t.Helper()
	session := os.Getenv("BUCKETMUX_MULTI_INSTANCE_UI_SESSION")
	if session == "" {
		t.Fatal("admin UI session is not available")
	}
	workDir := t.TempDir()
	keyJSON, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal UI delete key: %v", err)
	}
	code := fmt.Sprintf(`async page => {
		const key = %s
		if (!page.url().startsWith(%q)) await page.goto(%q)
		await page.locator('#object-browser-bucket').selectOption('images')
		await page.locator('#object-browser-prefix').fill(key)
		await page.locator('#object-browser-load').click()
		const row = page.locator('#object-browser-rows tr').filter({ hasText: key })
		await row.waitFor({ state: 'visible' })
		await row.getByRole('button', { name: 'Delete' }).click()
		await page.locator('#delete-object-dialog').waitFor({ state: 'visible' })
		await page.locator('#delete-object-confirmation').fill('Delete permanently')
		await page.locator('#delete-object-submit').click()
		await page.waitForFunction((key) => ![...document.querySelectorAll('#object-browser-rows tr')].some((row) => row.textContent.includes(key)), key)
	}`, keyJSON, baseURL, baseURL+"/admin")
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "run-code", code)
	runMultiInstancePlaywrightCLI(t, cli, session, workDir, "snapshot")
}

func runMultiInstancePlaywrightCLI(t *testing.T, cli, session, workDir string, args ...string) string {
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
