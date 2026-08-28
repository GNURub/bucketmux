package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
)

// tursoConnector initializes connection-local safety settings before a
// connection becomes visible to database/sql. PRAGMA foreign_keys is not a
// database-wide setting in Turso, so running it once on *sql.DB is insufficient
// when the pool opens additional connections.
type tursoConnector struct {
	driver driver.Driver
	dsn    string
}

func newTursoDB(dsn string) (*sql.DB, error) {
	probe, err := sql.Open("turso", dsn)
	if err != nil {
		return nil, err
	}
	underlying := probe.Driver()
	if err := probe.Close(); err != nil {
		return nil, err
	}
	return sql.OpenDB(&tursoConnector{driver: underlying, dsn: dsn}), nil
}

func (c *tursoConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	execer, ok := connection.(driver.ExecerContext)
	if !ok {
		_ = connection.Close()
		return nil, fmt.Errorf("turso driver connection does not implement driver.ExecerContext")
	}
	if _, err := execer.ExecContext(ctx, `PRAGMA foreign_keys = ON`, nil); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("enable Turso foreign keys on pooled connection: %w", err)
	}
	return connection, nil
}

func (c *tursoConnector) Driver() driver.Driver { return c.driver }
