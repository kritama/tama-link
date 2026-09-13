package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
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

func TestOpenMigratesLegacyEncryptedBlobsInBoundedBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &migrationKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	inputs := make([]NewSubmission, 0, encryptionMigrationBatchSize+1)
	for index := 0; index < encryptionMigrationBatchSize+1; index++ {
		input := NewSubmission{
			ID: fmt.Sprintf("sub-%02d", index), ClientRequestID: fmt.Sprintf("req-%02d", index),
			Tool: "message", Strategy: "upstream_task", DescriptorDigest: "sha256:test",
			Arguments: []byte(fmt.Sprintf(`{"message":"private-%02d"}`, index)),
		}
		if _, err := s.CreateSubmission(context.Background(), input); err != nil {
			t.Fatalf("CreateSubmission %d: %v", index, err)
		}
		legacyArguments := sealLegacyForTest(t, s.cipher, input.Arguments)
		legacyEvents := sealLegacyForTest(t, s.cipher, []byte(`[]`))
		legacyResult := sealLegacyForTest(t, s.cipher, []byte(`{"is_error":false,"content":[]}`))
		if _, err := s.db.Exec(`
			UPDATE submissions SET args_enc = ?, events_enc = ?, result_enc = ?
			WHERE submission_id = ?`, legacyArguments, legacyEvents, legacyResult, input.ID); err != nil {
			t.Fatalf("write legacy blobs %d: %v", index, err)
		}
		inputs = append(inputs, input)
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
	for index, input := range inputs {
		got, err := reopened.GetSubmission(context.Background(), input.ID)
		if err != nil {
			t.Fatalf("GetSubmission %d: %v", index, err)
		}
		if string(got.Arguments) != string(input.Arguments) {
			t.Fatalf("arguments %d = %s, want %s", index, got.Arguments, input.Arguments)
		}
		if got.Events == nil || got.Result == nil {
			t.Fatalf("submission %d did not retain events and result", index)
		}
	}
	var format string
	if err := reopened.db.QueryRow("SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		t.Fatalf("read format: %v", err)
	}
	if format != "2" {
		t.Fatalf("encryption format = %q, want 2", format)
	}
}

func TestEncryptionMigrationRollsBackEveryBatchOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &migrationKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for index := range 2 {
		input := NewSubmission{
			ID: fmt.Sprintf("sub-%d", index), ClientRequestID: fmt.Sprintf("req-%d", index),
			Tool: "message", Strategy: "upstream_task", DescriptorDigest: "sha256:test",
			Arguments: []byte(fmt.Sprintf(`{"message":"private-%d"}`, index)),
		}
		if _, err := s.CreateSubmission(context.Background(), input); err != nil {
			t.Fatalf("CreateSubmission %d: %v", index, err)
		}
	}
	legacy := sealLegacyForTest(t, s.cipher, []byte(`{"message":"private-0"}`))
	if _, err := s.db.Exec("UPDATE submissions SET args_enc = ? WHERE submission_id = 'sub-0'", legacy); err != nil {
		t.Fatalf("write first legacy blob: %v", err)
	}
	if _, err := s.db.Exec("UPDATE submissions SET args_enc = ? WHERE submission_id = 'sub-1'", []byte("corrupt")); err != nil {
		t.Fatalf("write corrupt legacy blob: %v", err)
	}
	if _, err := s.db.Exec("UPDATE meta SET value = '1' WHERE key = ?", metaEncryptionFormat); err != nil {
		t.Fatalf("write legacy format: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); err == nil {
		t.Fatal("Open migrated a database containing a corrupt legacy blob")
	}
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var gotBlob []byte
	if err := db.QueryRow("SELECT args_enc FROM submissions WHERE submission_id = 'sub-0'").Scan(&gotBlob); err != nil {
		t.Fatalf("read rolled-back blob: %v", err)
	}
	if !bytes.Equal(gotBlob, legacy) {
		t.Fatal("migration retained an earlier batch after a later row failed")
	}
	var format string
	if err := db.QueryRow("SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		t.Fatalf("read rolled-back format: %v", err)
	}
	if format != "1" {
		t.Fatalf("encryption format after rollback = %q, want 1", format)
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
