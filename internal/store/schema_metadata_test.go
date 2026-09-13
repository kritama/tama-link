package store_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

func TestOpenClassifiesMissingEncryptionMetadataAsUnavailable(t *testing.T) {
	for _, metadataKey := range []string{"state_key_id", "encryption_format"} {
		t.Run(metadataKey, func(t *testing.T) {
			keys := newMemKeys()
			s, path := openTestStore(t, keys, newClock())
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
			db, err := sql.Open("sqlite", uri)
			if err != nil {
				t.Fatalf("open raw database: %v", err)
			}
			if _, err := db.Exec("DELETE FROM meta WHERE key = ?", metadataKey); err != nil {
				t.Fatalf("remove %s: %v", metadataKey, err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close raw database: %v", err)
			}

			_, err = store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
			if !errors.Is(err, store.ErrStateUnavailable) {
				t.Fatalf("Open without %s = %v, want ErrStateUnavailable", metadataKey, err)
			}
		})
	}
}
