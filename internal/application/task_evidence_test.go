package application

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

const failureMarker = "secret-failure-marker"
const failureExtension = "ext-marker-9f3a"
const cancelMarker = "cancel-snapshot-9f3a"

func TestTerminalEvidenceSurvivesReopenAndStaysHidden(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		extra := `"error":{"code":-32603,"message":"` + failureMarker + `","extension":"` + failureExtension + `"}`
		return http.StatusOK, taskState("failed", "2026-09-11T10:00:05Z", extra)
	}
	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keys := newMemKeys()
	cfg := fixtureConfigAt(t, &fakeTama{ts: up.ts}, limits.Default(), nil, nil, dbPath, keys)
	svc, st, _ := appFromConfig(t, cfg)
	out, appErr := svc.Submit(context.Background(), messageInput("evidence"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
	awaited, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: out.SubmissionID})
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	encoded, err := json.Marshal(awaited)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), failureMarker) || strings.Contains(string(encoded), failureExtension) {
		t.Fatalf("await exposed terminal evidence: %s", encoded)
	}
	if awaited.Error == nil || awaited.Error.Code != contract.CodeUpstreamExecutionFailed {
		t.Fatalf("await error = %+v", awaited.Error)
	}
	_ = st.Close()

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), failureMarker) || strings.Contains(string(raw), failureExtension) {
		t.Fatal("sqlite file contains plaintext terminal evidence")
	}
	reopened, err := store.Open(context.Background(), dbPath, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	sub, err := reopened.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sub.TerminalEvidence), failureExtension) {
		t.Fatalf("reopened evidence = %s", sub.TerminalEvidence)
	}
}

func TestCancellationEvidenceSurvivesReopen(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("cancelled", "2026-09-11T10:00:05Z", `"statusMessage":"`+cancelMarker+`"`)
	}
	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keys := newMemKeys()
	cfg := fixtureConfigAt(t, &fakeTama{ts: up.ts}, limits.Default(), nil, nil, dbPath, keys)
	svc, st, _ := appFromConfig(t, cfg)
	out, appErr := svc.Submit(context.Background(), messageInput("cancel-evidence"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusCancelled)
	_ = st.Close()
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cancelMarker) {
		t.Fatal("sqlite file contains plaintext cancellation evidence")
	}
	reopened, err := store.Open(context.Background(), dbPath, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	sub, err := reopened.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sub.TerminalEvidence), cancelMarker) || !strings.Contains(string(sub.TerminalEvidence), `"status":"cancelled"`) {
		t.Fatalf("reopened cancellation evidence = %s", sub.TerminalEvidence)
	}
}
