package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

const (
	openRetryLimit = 5 * time.Second
	openRetryDelay = 50 * time.Millisecond

	sqliteBusy   = 5
	sqliteLocked = 6
)

type configuringConnector struct {
	driver.Connector
}

func (c configuringConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := configureSQLite(ctx, connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func openSQLite(ctx context.Context, path *statePath) (*sql.DB, error) {
	base, err := sqlite.NewConnector(sqliteURI(sqlitePinnedPath(path)))
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path.name, err)
	}
	connector := configuringConnector{Connector: base}
	deadline := time.Now().Add(openRetryLimit)

	for {
		database := sql.OpenDB(connector)
		err = database.PingContext(ctx)
		if err == nil {
			return database, nil
		}
		_ = database.Close()
		if !isSQLiteContention(err) || !waitForOpenRetry(ctx, deadline) {
			return nil, fmt.Errorf("open state database %s: %w", path.name, err)
		}
	}
}

func configureSQLite(ctx context.Context, connection driver.Conn) error {
	execer, ok := connection.(driver.ExecerContext)
	if !ok {
		return fmt.Errorf("SQLite driver does not support connection configuration")
	}
	pragmas := []struct {
		name string
		sql  string
	}{
		{"busy timeout", "PRAGMA busy_timeout=5000"},
		{"journal mode", "PRAGMA journal_mode=WAL"},
		{"synchronous mode", "PRAGMA synchronous=NORMAL"},
	}
	for _, pragma := range pragmas {
		if _, err := execer.ExecContext(ctx, pragma.sql, nil); err != nil {
			return fmt.Errorf("configure SQLite %s: %w", pragma.name, err)
		}
	}
	return nil
}

func sqliteURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func isSQLiteContention(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		return true
	default:
		return false
	}
}

func waitForOpenRetry(ctx context.Context, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(min(openRetryDelay, remaining))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
