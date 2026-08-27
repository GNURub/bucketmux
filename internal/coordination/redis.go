package coordination

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type Coordinator interface {
	TryAcquire(ctx context.Context, name string, ttl time.Duration) (Lease, bool, error)
	Ping(ctx context.Context) error
}

type Lease interface {
	Release(ctx context.Context) error
}

type NoopCoordinator struct{}

func (NoopCoordinator) TryAcquire(context.Context, string, time.Duration) (Lease, bool, error) {
	return noopLease{}, true, nil
}

func (NoopCoordinator) Ping(context.Context) error { return nil }

type noopLease struct{}

func (noopLease) Release(context.Context) error { return nil }

type RedisConfig struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string
}

type RedisCoordinator struct {
	cfg RedisConfig
}

const releaseLeaseScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

func NewRedis(cfg RedisConfig) *RedisCoordinator {
	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		cfg.KeyPrefix = "bucketmux"
	}
	return &RedisCoordinator{cfg: cfg}
}

func (c *RedisCoordinator) TryAcquire(ctx context.Context, name string, ttl time.Duration) (Lease, bool, error) {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	key := c.cfg.KeyPrefix + ":worker:" + name
	reply, err := c.command(ctx, "SET", key, token, "NX", "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
	if err != nil {
		return nil, false, err
	}
	if reply != "+OK" {
		return nil, false, nil
	}
	return redisLease{coordinator: c, key: key, token: token}, true, nil
}

func (c *RedisCoordinator) Ping(ctx context.Context) error {
	reply, err := c.command(ctx, "PING")
	if err != nil {
		return err
	}
	if reply != "+PONG" {
		return fmt.Errorf("unexpected redis PING response %q", reply)
	}
	return nil
}

type redisLease struct {
	coordinator *RedisCoordinator
	key         string
	token       string
}

func (l redisLease) Release(ctx context.Context) error {
	_, err := l.coordinator.command(ctx, "EVAL", releaseLeaseScript, "1", l.key, l.token)
	return err
}

func (c *RedisCoordinator) command(ctx context.Context, args ...string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	if c.cfg.Password != "" {
		if err := writeRESP(conn, "AUTH", c.cfg.Password); err != nil {
			return "", err
		}
		if _, err := readRESP(reader); err != nil {
			return "", err
		}
	}
	if c.cfg.DB > 0 {
		if err := writeRESP(conn, "SELECT", strconv.Itoa(c.cfg.DB)); err != nil {
			return "", err
		}
		if _, err := readRESP(reader); err != nil {
			return "", err
		}
	}
	if err := writeRESP(conn, args...); err != nil {
		return "", err
	}
	return readRESP(reader)
}

func writeRESP(conn net.Conn, args ...string) error {
	if _, err := fmt.Fprintf(conn, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return nil
}

func readRESP(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if after, ok := strings.CutPrefix(line, "-"); ok {
		return "", fmt.Errorf("redis error: %s", after)
	}
	if !strings.HasPrefix(line, "$") {
		return line, nil
	}
	size, err := strconv.Atoi(strings.TrimPrefix(line, "$"))
	if err != nil || size < 0 {
		return line, err
	}
	buf := make([]byte, size+2)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	return line + "\n" + string(buf[:size]), nil
}

func randomToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
