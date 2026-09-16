package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/limits"
)

func TestUnreadableEncryptedPayloadsReportStateUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{name: "arguments", query: "UPDATE submissions SET args_enc = ? WHERE submission_id = ?"},
		{name: "events", query: "UPDATE submissions SET events_enc = ? WHERE submission_id = ?"},
		{name: "result", query: "UPDATE submissions SET result_enc = ? WHERE submission_id = ?"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), &testKeys{}, Config{
				Limits: limits.Default(),
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			input := NewSubmission{
				ID: "sub-1", ClientRequestID: "req-1", Tool: "message",
				Strategy: "upstream_task", DescriptorDigest: "sha256:test",
				Arguments:        []byte(`{"message":"private"}`),
				RequestArguments: []byte(`{"message":"private"}`),
			}
			if _, _, err := s.CreateSubmission(context.Background(), input); err != nil {
				t.Fatalf("CreateSubmission: %v", err)
			}
			if _, err := s.db.Exec(test.query, []byte("corrupt"), input.ID); err != nil {
				t.Fatalf("corrupt %s: %v", test.name, err)
			}

			if _, err := s.GetSubmission(context.Background(), input.ID); !errors.Is(err, ErrStateUnavailable) {
				t.Fatalf("GetSubmission with corrupt %s = %v, want ErrStateUnavailable", test.name, err)
			}
		})
	}
}
