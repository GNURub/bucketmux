.PHONY: run-local test test-bun test-seaweedfs

run-local:
	mkdir -p data
	CONFIG_PATH=config.local.yaml MASTER_KEY=replace-with-a-long-random-secret go run ./cmd/bucketmux

test:
	GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./...

test-bun:
	BUCKETMUX_RUN_BUN_INTEGRATION=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/gateway -run TestBunS3ClientCompatibility -count=1 -v

test-seaweedfs:
	BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION=1 GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./internal/provider -run TestSeaweedFSS3CompatibleProvider -count=1 -v
