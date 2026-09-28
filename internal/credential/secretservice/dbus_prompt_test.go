package secretservice

import (
	"errors"
	"strings"
	"testing"

	"github.com/godbus/dbus"
)

func TestPromptCompletedDoesNotEchoResult(t *testing.T) {
	t.Parallel()

	secret := "refresh-token-SUPER-SECRET"
	_, err := promptCompleted([]interface{}{true, dbus.MakeVariant(secret)})
	if !errors.Is(err, ErrPromptDismissed) {
		t.Fatalf("dismissed prompt = %v, want ErrPromptDismissed", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("prompt error %q contains secret material", err)
	}
}

func TestPromptCompletedRejectsMalformedResult(t *testing.T) {
	t.Parallel()

	if _, err := promptCompleted(nil); err == nil {
		t.Fatal("empty prompt result succeeded")
	}
	if _, err := promptCompleted([]interface{}{"no"}); err == nil {
		t.Fatal("non-bool dismissed flag succeeded")
	}
	result, err := promptCompleted([]interface{}{false, dbus.MakeVariant("ok")})
	if err != nil {
		t.Fatalf("accepted prompt = %v", err)
	}
	if result.Value() != "ok" {
		t.Fatalf("prompt result = %#v", result.Value())
	}
}

func TestIsPromptIgnoresRootPath(t *testing.T) {
	t.Parallel()

	if isPrompt("/") || isPrompt("") {
		t.Fatal("root path was treated as a prompt")
	}
	if !isPrompt(dbus.ObjectPath(dbusServicePath + "/prompt/p0")) {
		t.Fatal("prompt path was not recognized")
	}
}
