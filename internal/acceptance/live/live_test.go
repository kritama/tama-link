//go:build live

package live

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/acceptance"
)

// TestLiveAcceptance is an unimplemented scaffold. It fails closed and does
// not start Tama Link, perform OAuth, exercise either profile, restart Link
// or Tama, or write evidence. A pass is not possible in this revision and
// must not be read as live acceptance. The runner belongs here only after
// the migrated topology exists.
func TestLiveAcceptance(t *testing.T) {
	missing := missingEnv(
		"TAMA_LINK_LIVE",
		"TAMA_LINK_LIVE_APP_ENDPOINT",
		"TAMA_LINK_LIVE_SYSTEM_ENDPOINT",
		"TAMA_LINK_LIVE_TAMAMCP_SHA",
		"TAMA_LINK_LIVE_TAMA_SHA",
		"TAMA_LINK_LIVE_PROVIDER_SHA",
	)
	if len(missing) > 0 {
		t.Fatalf("live acceptance is not runnable:\n- %s\nfixture and mocked results are not live acceptance", strings.Join(missing, "\n- "))
	}
	if os.Getenv("TAMA_LINK_LIVE") != "1" {
		t.Fatal("TAMA_LINK_LIVE must be 1; fixture results are not live acceptance")
	}
	for _, name := range []string{
		"TAMA_LINK_LIVE_TAMAMCP_SHA",
		"TAMA_LINK_LIVE_TAMA_SHA",
		"TAMA_LINK_LIVE_PROVIDER_SHA",
	} {
		if !acceptance.RevisionOK(os.Getenv(name)) {
			t.Fatalf("%s is not a SHA or release identifier", name)
		}
	}
	t.Fatal(unimplementedChecks())
}

func missingEnv(names ...string) []string {
	var missing []string
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name+" is not set")
		}
	}
	return missing
}

func unimplementedChecks() error {
	return fmt.Errorf("live acceptance has not passed: %s\nupmaru/tama#123 and the System/App/PubSub migration are not recorded, so this command does not write compatibility evidence", strings.Join(acceptance.RequiredChecks(), ", "))
}
