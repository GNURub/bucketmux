package coordination

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRedisLeaseReleaseUsesAtomicCompareAndDelete(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	commands := make(chan []string, 1)
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer func() { _ = conn.Close() }()
		args, err := readTestRESPCommand(bufio.NewReader(conn))
		if err != nil {
			serverErrors <- err
			return
		}
		commands <- args
		_, err = io.WriteString(conn, ":1\r\n")
		serverErrors <- err
	}()

	coordinator := NewRedis(RedisConfig{Addr: listener.Addr().String()})
	lease := redisLease{coordinator: coordinator, key: "bucketmux:worker:hooks:claim", token: "owner-token"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	want := []string{"EVAL", releaseLeaseScript, "1", lease.key, lease.token}
	select {
	case got := <-commands:
		if !slices.Equal(got, want) {
			t.Fatalf("Redis command = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Redis command")
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake Redis server error = %v", err)
	}
}

func TestRandomTokenIsCryptographicallySized(t *testing.T) {
	token, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken() error = %v", err)
	}
	if len(token) != 32 {
		t.Fatalf("randomToken() length = %d, want 32", len(token))
	}
}

func TestRedisCoordinatorIntegration(t *testing.T) {
	if os.Getenv("BUCKETMUX_RUN_REDIS_INTEGRATION") != "1" {
		t.Skip("set BUCKETMUX_RUN_REDIS_INTEGRATION=1 to run")
	}
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Fatal("REDIS_ADDR is required")
	}
	coordinator := NewRedis(RedisConfig{Addr: addr, KeyPrefix: "bucketmux-integration"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lease, acquired, err := coordinator.TryAcquire(ctx, "atomic-release", 10*time.Second)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() = %v, %v", acquired, err)
	}
	if _, acquired, err := coordinator.TryAcquire(ctx, "atomic-release", 10*time.Second); err != nil || acquired {
		t.Fatalf("second TryAcquire() = %v, %v, want not acquired", acquired, err)
	}
	redisOwnedLease := lease.(redisLease)
	if _, err := coordinator.command(ctx, "SET", redisOwnedLease.key, "new-owner"); err != nil {
		t.Fatalf("replace lease owner: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	reply, err := coordinator.command(ctx, "GET", redisOwnedLease.key)
	if err != nil {
		t.Fatalf("GET replacement owner error = %v", err)
	}
	if !strings.HasSuffix(reply, "\nnew-owner") {
		t.Fatalf("replacement owner was deleted: %q", reply)
	}
}

func readTestRESPCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, fmt.Errorf("parse command length: %w", err)
	}
	args := make([]string, 0, count)
	for range count {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil {
			return nil, fmt.Errorf("parse argument length: %w", err)
		}
		value := make([]byte, size+2)
		if _, err := io.ReadFull(reader, value); err != nil {
			return nil, err
		}
		args = append(args, string(value[:size]))
	}
	return args, nil
}
