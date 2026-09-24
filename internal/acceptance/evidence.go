package acceptance

import (
	"fmt"
	"slices"
	"strings"
)

// GateLive is the only evidence gate that may be treated as deployment or
// runtime acceptance. Fixture and compose-pin results use other names and
// are not substitutes.
const GateLive = "live"

func requiredChecks() [12]string {
	return [...]string{
		"oauth",
		"owner_isolation",
		"input_update",
		"notification_delivery",
		"polling_recovery",
		"terminal_success",
		"terminal_failure",
		"link_restart",
		"tama_restart",
		"expired_credentials",
		"ambiguous_replay",
		"system_recovery",
	}
}

// RequiredChecks returns a copy of the live observations Issue #8 requires.
func RequiredChecks() []string {
	checks := requiredChecks()
	return checks[:]
}

// TaskRecovery is the App restart or ambiguous-replay observation. Graph
// executions must stay at one even when a later call is retried.
type TaskRecovery struct {
	TaskID             string `json:"task_id"`
	TaskIDAfterRestart string `json:"task_id_after_restart"`
	CallAttempts       int    `json:"call_attempts"`
	GraphExecutions    int    `json:"graph_executions"`
}

// SystemRecovery is the read-only System replay observation.
type SystemRecovery struct {
	Replays            int `json:"replays"`
	ProtectedMutations int `json:"protected_mutations"`
}

// ForbiddenWire records each forbidden upstream shape separately.
type ForbiddenWire struct {
	Methods    []string `json:"methods"`
	SessionID  bool     `json:"session_id"`
	ParamsTask bool     `json:"params_task"`
}

// Evidence is the live acceptance record. It is written only after every
// required check passes against the migrated runtime. Fixture runs must not
// create it.
type Evidence struct {
	Gate            string          `json:"gate"`
	TamaMCPSHA      string          `json:"tamamcp_sha"`
	TamaSHA         string          `json:"tama_sha"`
	ProviderSHA     string          `json:"provider_sha"`
	Profiles        []string        `json:"profiles"`
	Checks          map[string]bool `json:"checks"`
	AppRestart      TaskRecovery    `json:"app_restart"`
	AmbiguousReplay TaskRecovery    `json:"ambiguous_replay"`
	SystemRecovery  SystemRecovery  `json:"system_recovery"`
	ForbiddenWire   ForbiddenWire   `json:"forbidden_wire"`
}

// Validate reports whether a record is complete live evidence. It rejects
// fixture-shaped documents and partial check maps.
func Validate(e Evidence) error {
	if e.Gate != GateLive {
		return fmt.Errorf("gate %q is not live acceptance", e.Gate)
	}
	if !RevisionOK(e.TamaMCPSHA) || !RevisionOK(e.TamaSHA) || !RevisionOK(e.ProviderSHA) {
		return fmt.Errorf("live evidence requires TamaMCP, Tama, and provider revisions")
	}
	if !slices.Contains(e.Profiles, "app") || !slices.Contains(e.Profiles, "system") {
		return fmt.Errorf("live evidence requires app and system profiles")
	}
	if err := validateRecovery(e); err != nil {
		return err
	}
	var missing []string
	for _, check := range requiredChecks() {
		if !e.Checks[check] {
			missing = append(missing, check)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("live evidence missing checks: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateRecovery(e Evidence) error {
	if err := validateTaskRecovery("app restart", e.AppRestart, 1); err != nil {
		return err
	}
	if err := validateTaskRecovery("ambiguous replay", e.AmbiguousReplay, 2); err != nil {
		return err
	}
	if e.AmbiguousReplay.GraphExecutions != 1 || e.AmbiguousReplay.CallAttempts <= e.AmbiguousReplay.GraphExecutions {
		return fmt.Errorf("ambiguous replay did not prove one graph execution across retried calls")
	}
	if e.SystemRecovery.Replays < 1 || e.SystemRecovery.ProtectedMutations != 0 {
		return fmt.Errorf("system recovery did not prove a read-only replay")
	}
	if len(e.ForbiddenWire.Methods) != 0 || e.ForbiddenWire.SessionID || e.ForbiddenWire.ParamsTask {
		return fmt.Errorf("live evidence observed a forbidden upstream method, session header, or params.task")
	}
	return nil
}

func validateTaskRecovery(name string, recovery TaskRecovery, minCalls int) error {
	if recovery.TaskID == "" || recovery.TaskID != recovery.TaskIDAfterRestart {
		return fmt.Errorf("%s did not keep a stable owner-bound task id", name)
	}
	if recovery.GraphExecutions != 1 || recovery.CallAttempts < minCalls {
		return fmt.Errorf("%s call attempts %d and graph executions %d do not prove a single graph execution", name, recovery.CallAttempts, recovery.GraphExecutions)
	}
	return nil
}

// RevisionOK accepts a git SHA prefix or a SemVer release identifier.
func RevisionOK(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\n") {
		return false
	}
	if hexSHA(value) {
		return len(value) >= 7
	}
	return semverRelease(value)
}

func hexSHA(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return value != ""
}

func semverRelease(value string) bool {
	value = strings.TrimPrefix(value, "v")
	core, build, ok := strings.Cut(value, "+")
	if ok && (!validIdentifiers(build, false) || strings.Contains(build, "+")) {
		return false
	}
	core, prerelease, hasPrerelease := strings.Cut(core, "-")
	if hasPrerelease && !validIdentifiers(prerelease, true) {
		return false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !validNumericIdentifier(part, true) {
			return false
		}
	}
	return true
}

func validIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, r := range identifier {
			if r < '0' || r > '9' {
				numeric = false
			}
			if !validIdentifierRune(r) {
				return false
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validIdentifierRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') || r == '-'
}

func validNumericIdentifier(value string, rejectLeadingZero bool) bool {
	if value == "" || (rejectLeadingZero && len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
