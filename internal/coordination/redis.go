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
}

type Lease interface {
	Release(ctx context.Context) error
}

type NoopCoordinator struct{}

func (NoopCoordinator) TryAcquire(context.Context, string, time.Duration) (Lease, bool, error) {
	return noopLease{}, true, nil
}

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
	token := randomToken()
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

type redisLease struct {
	coordinator *RedisCoordinator
	key         string
	token       string
}

func (l redisLease) Release(ctx context.Context) error {
	reply, err := l.coordinator.command(ctx, "GET", l.key)
	if err != nil || !strings.HasPrefix(reply, "$") || !strings.HasSuffix(reply, "\n"+l.token) {
		return err
	}
	_, err = l.coordinator.command(ctx, "DEL", l.key)
	return err
}

func (c *RedisCoordinator) command(ctx context.Context, args ...string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
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
	if strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("redis error: %s", strings.TrimPrefix(line, "-"))
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

func randomToken() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(bytes[:])
}
