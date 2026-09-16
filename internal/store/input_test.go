package store_test

import (
	"context"
	"encoding/json"
	"testing"
)

func TestInputResponseRoundTrip(t *testing.T) {
	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()

	if _, found, err := s.GetInputResponse(ctx, "sub-1", "approval"); err != nil || found {
		t.Fatalf("missing record: found=%v err=%v", found, err)
	}

	resp := json.RawMessage(`{"action":"accept","content":{"approved":true}}`)
	if err := s.SetInputResponse(ctx, "sub-1", "approval", resp); err != nil {
		t.Fatalf("SetInputResponse: %v", err)
	}
	got, found, err := s.GetInputResponse(ctx, "sub-1", "approval")
	if err != nil || !found {
		t.Fatalf("GetInputResponse: found=%v err=%v", found, err)
	}
	if string(got) != string(resp) {
		t.Fatalf("response = %s, want %s", got, resp)
	}

	// A second write for the same ID never replaces the first record.
	if err := s.SetInputResponse(ctx, "sub-1", "approval", json.RawMessage(`{"action":"deny"}`)); err != nil {
		t.Fatalf("second SetInputResponse: %v", err)
	}
	got, found, err = s.GetInputResponse(ctx, "sub-1", "approval")
	if err != nil || !found || string(got) != string(resp) {
		t.Fatalf("record mutated: %s found=%v err=%v", got, found, err)
	}

	// Distinct submissions are isolated.
	if _, found, err := s.GetInputResponse(ctx, "sub-2", "approval"); err != nil || found {
		t.Fatalf("cross-submission leak: found=%v err=%v", found, err)
	}
}

func TestInputResponseValidation(t *testing.T) {
	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()

	if err := s.SetInputResponse(ctx, "", "a", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty submission id accepted")
	}
	if err := s.SetInputResponse(ctx, "s", "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty request id accepted")
	}
	if err := s.SetInputResponse(ctx, "s", "a", json.RawMessage(nil)); err == nil {
		t.Fatal("empty response accepted")
	}
}
