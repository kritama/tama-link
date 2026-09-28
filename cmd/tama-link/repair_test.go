package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
	"github.com/kritama/tama-link/internal/repair"
)

func TestRepairUnsupportedMessage(t *testing.T) {
	t.Parallel()

	if repairUnsupported("linux") != "" {
		t.Fatal("linux was reported unsupported")
	}
	message := repairUnsupported("darwin")
	if !strings.Contains(message, "darwin") || strings.Contains(message, "secret") {
		t.Fatalf("message = %q", message)
	}
}

func TestRepairDoesNotPrintSecrets(t *testing.T) {
	const secret = "refresh-token-SUPER-SECRET"
	configDir := t.TempDir()
	if err := writeDemoProfile(configDir); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	service := secrettest.New()
	service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink", secretservice.LegacyCollectionLabel).
		Put("demo/default/unrelated", []byte(secret), "text/plain")

	previous := dialRepairService
	dialRepairService = func(context.Context) (secretservice.Service, error) { return service, nil }
	t.Cleanup(func() { dialRepairService = previous })

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"repair", "--profile", "demo", "--config-dir", configDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, stderr %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr %q contains secret material", stderr.String())
	}
	if !strings.Contains(stderr.String(), "repair failed") {
		t.Fatalf("stderr = %q, want repair failed", stderr.String())
	}
}

func TestRepairBoundsDialToInteractionBudget(t *testing.T) {
	configDir := t.TempDir()
	if err := writeDemoProfile(configDir); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	previous := dialRepairService
	dialRepairService = func(ctx context.Context) (secretservice.Service, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errDialDeadline
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > repair.InteractionBudget {
			return nil, errDialDeadline
		}
		return nil, errDialStopped
	}
	t.Cleanup(func() { dialRepairService = previous })

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"repair", "--profile", "demo", "--config-dir", configDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), errDialDeadline.Error()) {
		t.Fatalf("dial context was not bounded: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), errDialStopped.Error()) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

var (
	errDialDeadline = errString("dial context is outside the repair budget")
	errDialStopped  = errString("dial stopped")
)

type errString string

func (e errString) Error() string { return string(e) }

func TestRepairRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"repair", "--profile", "demo", "extra"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "no positional arguments") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
