package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/limits"
)

func TestOpenMigratesSchemaVersionOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	legacySchema := strings.Replace(createSchema,
		"    error_retryable INTEGER NOT NULL DEFAULT 0,\n", "", 1)
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	keys := &migrationKeys{key: make([]byte, 32)}
	for key, value := range map[string]string{
		metaSchemaVersion:    "1",
		metaEncryptionFormat: "2",
		metaStateKeyID:       "state",
	} {
		if _, err := db.Exec("INSERT INTO meta (key, value) VALUES (?, ?)", key, value); err != nil {
			t.Fatalf("insert metadata: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open with migration: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.schema != schemaVersion {
		t.Fatalf("schema = %d, want %d", s.schema, schemaVersion)
	}
	var columns int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM pragma_table_info('submissions')
		WHERE name = 'error_retryable'`).Scan(&columns); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if columns != 1 {
		t.Fatalf("error_retryable columns = %d, want 1", columns)
	}
}
