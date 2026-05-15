package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

func TestBunS3ClientCompatibility(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_BUN_INTEGRATION") != "1" {
		t.Skip("set BUCKETMUX_RUN_BUN_INTEGRATION=1 to run Bun S3Client integration test")
	}
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	dataDir := t.TempDir()
	svc, err := app.NewService(context.Background(), config.Config{
		Server:  config.ServerConfig{Addr: ":0", DataDir: dataDir, DBPath: filepath.Join(dataDir, "switcher.db"), MasterKey: "test-master-key"},
		S3:      config.S3Config{AccessKey: "local-access-key", SecretKey: "local-secret-key", Region: "auto"},
		Buckets: []config.BucketConfig{{Name: "images"}},
		Providers: []config.ProviderConfig{{
			ID:            "local-test",
			Name:          "Local test",
			Kind:          string(domain.ProviderKindLocal),
			Bucket:        "images",
			CapacityBytes: 1024 * 1024 * 1024,
			Priority:      1,
			Enabled:       boolPtr(true),
			Settings:      map[string]string{"path": filepath.Join(dataDir, "objects")},
		}},
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer svc.Close()

	server := newTestServer(NewHandler(svc))
	defer server.Close()

	script := `
import { S3Client } from "bun";

const endpoint = process.env.BUCKETMUX_ENDPOINT;
const credentials = {
  endpoint,
  bucket: "images",
  accessKeyId: "local-access-key",
  secretAccessKey: "local-secret-key",
  region: "auto",
};
const client = new S3Client(credentials);

const file = client.file("bun/hello.txt");
await file.write("hello from bun", { type: "text/plain" });
if ((await file.text()) !== "hello from bun") throw new Error("text read failed");
if (!(await file.exists())) throw new Error("exists failed");
const stat = await file.stat();
if (stat.size !== 14) throw new Error("stat size failed: " + stat.size);
const sliced = await file.slice(6, 10).text();
if (sliced !== "from") throw new Error("slice/range failed: " + sliced);
const presigned = file.presign({ expiresIn: 60 });
const presignedRes = await fetch(presigned);
if (!presignedRes.ok) throw new Error("presign fetch failed: " + presignedRes.status + " " + await presignedRes.text());
if ((await presignedRes.text()) !== "hello from bun") throw new Error("presign body failed");

const listed = await client.list({ prefix: "bun/", maxKeys: 100, fetchOwner: true });
if (!listed.contents?.some((entry) => entry.key === "bun/hello.txt")) throw new Error("list failed: " + JSON.stringify(listed));

const writerFile = client.file("bun/writer.txt");
const writer = writerFile.writer({ type: "text/plain", partSize: 5 * 1024 * 1024, queueSize: 2 });
writer.write("streamed ");
writer.write("from writer");
await writer.end();
if ((await writerFile.text()) !== "streamed from writer") throw new Error("writer failed");

await file.delete();
if (await file.exists()) throw new Error("delete failed");
await writerFile.unlink();
console.log("bun-s3-ok");
`
	scriptPath := filepath.Join(t.TempDir(), "bun-s3-compat.ts")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write bun script: %v", err)
	}
	cmd := exec.Command(bunPath, scriptPath)
	cmd.Env = append(os.Environ(), "BUCKETMUX_ENDPOINT="+server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bun S3 compatibility failed: %v\n%s", err, output)
	}
}

func newTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}
