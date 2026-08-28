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
	for _, pragma := range []struct {
		query       string
		description string
	}{
		{query: `PRAGMA foreign_keys = ON`, description: "foreign keys"},
		{query: `PRAGMA synchronous = NORMAL`, description: "normal synchronous mode"},
		{query: `PRAGMA busy_timeout = 5000`, description: "busy timeout"},
	} {
		if _, err := execer.ExecContext(ctx, pragma.query, nil); err != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("configure Turso %s on pooled connection: %w", pragma.description, err)
		}
	}
	return connection, nil
}

func (c *tursoConnector) Driver() driver.Driver { return c.driver }
