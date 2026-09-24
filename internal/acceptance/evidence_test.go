package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveEvidenceIsNotImplied(t *testing.T) {
	path := filepath.Join("..", "..", "wip", "phase-2-live-evidence.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var record Evidence
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("live evidence file is not a record: %v", err)
	}
	if err := Validate(record); err != nil {
		t.Fatalf("wip/phase-2-live-evidence.json is present but is not live acceptance: %v", err)
	}
}

func TestValidateRejectsFixtureShapedEvidence(t *testing.T) {
	record := Evidence{
		Gate:        "fixture",
		TamaMCPSHA:  "6b5db00018d2774834db5a0f00eed5b9b55e1d2e",
		TamaSHA:     "unmigrated",
		ProviderSHA: "unmigrated",
		Profiles:    []string{"app", "system"},
		Checks:      map[string]bool{},
	}
	for _, check := range RequiredChecks {
		record.Checks[check] = true
	}
	if err := Validate(record); err == nil {
		t.Fatal("fixture gate was accepted as live evidence")
	}
}

func TestRevisionOKAcceptsSHAOrRelease(t *testing.T) {
	if !RevisionOK("5c80c29") || !RevisionOK("v0.2.0") {
		t.Fatal("sha prefix or release identifier rejected")
	}
	if RevisionOK("latest") || RevisionOK("abc") || RevisionOK("") {
		t.Fatal("floating or empty revision accepted")
	}
}

func TestValidateAcceptsCompleteLiveEvidence(t *testing.T) {
	if err := Validate(completeEvidence()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsChecksWithoutRecoveryInvariants(t *testing.T) {
	record := completeEvidence()
	record.AmbiguousReplay.GraphExecutions = 2
	if err := Validate(record); err == nil {
		t.Fatal("duplicate graph execution accepted")
	}
	record = completeEvidence()
	record.SystemRecovery.ProtectedMutations = 1
	if err := Validate(record); err == nil {
		t.Fatal("protected system mutation accepted")
	}
	record = completeEvidence()
	record.ForbiddenWire.SessionID = true
	if err := Validate(record); err == nil {
		t.Fatal("session header observation accepted")
	}
	record = completeEvidence()
	record.ForbiddenWire.ParamsTask = true
	if err := Validate(record); err == nil {
		t.Fatal("params.task observation accepted")
	}
}

func completeEvidence() Evidence {
	checks := map[string]bool{}
	for _, check := range RequiredChecks {
		checks[check] = true
	}
	return Evidence{
		Gate:        GateLive,
		TamaMCPSHA:  "5c80c29e90c49438fbcc331db5c00f9f8f93ee21",
		TamaSHA:     "abcdef0",
		ProviderSHA: "v0.4.1",
		Profiles:    []string{"app", "system"},
		Checks:      checks,
		AppRestart: TaskRecovery{
			TaskID: "task-1", TaskIDAfterRestart: "task-1", CallAttempts: 1, GraphExecutions: 1,
		},
		AmbiguousReplay: TaskRecovery{
			TaskID: "task-1", TaskIDAfterRestart: "task-1", CallAttempts: 2, GraphExecutions: 1,
		},
		SystemRecovery: SystemRecovery{Replays: 1, ProtectedMutations: 0},
	}
}

func TestValidateRequiresBothProfilesAndEveryCheck(t *testing.T) {
	record := Evidence{
		Gate:        GateLive,
		TamaMCPSHA:  "5c80c29e90c49438fbcc331db5c00f9f8f93ee21",
		TamaSHA:     "abc1234",
		ProviderSHA: "v0.4.1",
		Profiles:    []string{"app"},
		Checks:      map[string]bool{},
	}
	if err := Validate(record); err == nil {
		t.Fatal("system profile was not required")
	}
	record.Profiles = []string{"app", "system"}
	if err := Validate(record); err == nil {
		t.Fatal("missing checks were accepted")
	}
}
