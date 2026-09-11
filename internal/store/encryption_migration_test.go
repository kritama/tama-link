package store

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/limits"
)

type migrationKeys struct {
	key []byte
}

func (m *migrationKeys) GetStateKey(string) ([]byte, bool, error) {
	return m.key, len(m.key) != 0, nil
}

func (m *migrationKeys) CreateStateKey() (string, []byte, error) {
	m.key = make([]byte, 32)
	if _, err := rand.Read(m.key); err != nil {
		return "", nil, err
	}
	return "state", m.key, nil
}

func TestOpenMigratesLegacyEncryptedBlobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &migrationKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	input := NewSubmission{
		ID: "sub-1", ClientRequestID: "req-1", Tool: "message", Strategy: "upstream_task",
		DescriptorDigest: "sha256:test", Arguments: []byte(`{"message":"private"}`),
	}
	if _, err := s.CreateSubmission(context.Background(), input); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	legacy := sealLegacyForTest(t, s.cipher, input.Arguments)
	if _, err := s.db.Exec("UPDATE submissions SET args_enc = ? WHERE submission_id = ?", legacy, input.ID); err != nil {
		t.Fatalf("write legacy blob: %v", err)
	}
	if _, err := s.db.Exec("UPDATE meta SET value = '1' WHERE key = ?", metaEncryptionFormat); err != nil {
		t.Fatalf("write legacy format: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("reopen with migration: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetSubmission(context.Background(), input.ID)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if string(got.Arguments) != string(input.Arguments) {
		t.Fatalf("arguments = %s, want %s", got.Arguments, input.Arguments)
	}
	var format string
	if err := reopened.db.QueryRow("SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		t.Fatalf("read format: %v", err)
	}
	if format != "2" {
		t.Fatalf("encryption format = %q, want 2", format)
	}
}

func sealLegacyForTest(t *testing.T, cipher stateCipher, plaintext []byte) []byte {
	t.Helper()
	nonce := make([]byte, cipher.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return cipher.aead.Seal(nonce, nonce, plaintext, []byte("tama-link/blob/v1"))
}
