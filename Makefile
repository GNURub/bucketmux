.PHONY: run-local build test test-race test-turso-vector-scale lint vulncheck ci backup-postgres restore-postgres test-bun test-fetch test-imgproxy test-k6 test-migration test-ministack test-multi-instance test-uppy test-seaweedfs test-redis test-wasm-plugins test-gemini-image-embeddings

run-local:
	mkdir -p data
	CONFIG_PATH=config.local.yaml MASTER_KEY=replace-with-a-long-random-secret go run ./cmd/bucketmux

build:
	CGO_ENABLED=0 go build -trimpath -o /tmp/bucketmux ./cmd/bucketmux

test:
	GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./...

test-race:
	GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test -race ./...

test-turso-vector-scale:
	BUCKETMUX_RUN_TURSO_SCALE=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/store -run '^TestTursoNativeVectorScaleIntegration$$' -count=1 -v

lint:
	go vet ./...
	staticcheck ./...
	golangci-lint run

vulncheck:
	govulncheck ./...

ci: test test-race lint vulncheck build

backup-postgres:
	test -n "$(BACKUP_FILE)"
	sh scripts/backup-postgres.sh "$(BACKUP_FILE)"

restore-postgres:
	test -n "$(BACKUP_FILE)" && test -n "$(TARGET_DATABASE)"
	sh scripts/restore-postgres.sh "$(BACKUP_FILE)" "$(TARGET_DATABASE)"

test-bun:
	BUCKETMUX_RUN_BUN_INTEGRATION=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/gateway -run TestBunS3ClientCompatibility -count=1 -v

test-fetch:
	sh scripts/test-fetch-browser-e2e.sh

test-imgproxy:
	sh scripts/test-imgproxy-e2e.sh

test-k6:
	sh scripts/test-k6-performance.sh

test-migration:
	sh scripts/test-migration-e2e.sh

test-ministack:
	sh scripts/test-ministack-e2e.sh

test-multi-instance:
	sh scripts/test-multi-instance-e2e.sh

test-uppy:
	sh scripts/test-uppy-browser-e2e.sh

test-seaweedfs:
	BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/provider -run TestSeaweedFSS3CompatibleProvider -count=1 -v

test-redis:
	BUCKETMUX_RUN_REDIS_INTEGRATION=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/coordination -run TestRedisCoordinatorIntegration -count=1 -v

test-wasm-plugins:
	./examples/wasm/go/build.sh
	./examples/wasm/rust/build.sh
	cd examples/wasm/bun-assemblyscript && bun install --frozen-lockfile && bun run build && bun test
	BUCKETMUX_RUN_WASM_EXAMPLES=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/wasmplugin ./internal/app ./internal/admin -run 'TestGoBuiltWASMExamples|TestRustAndBunBuiltWASMExamples|TestGoEmbeddingPipelineSingleAndMultipleInstances|TestWASMPipelineSingleAndMultipleInstances|TestWASMDeclarativeBucketOperationsSingleAndMultipleInstances|TestWASMOperationsDenyByDefaultAndRespectObjectLock|TestWASMPluginAdminAPIInstallsRustGuest|TestEmbeddingAdminAPIListsWithoutValuesAndSearches' -count=1 -v

test-gemini-image-embeddings:
	cd examples/gemini-image-embeddings && bun test
