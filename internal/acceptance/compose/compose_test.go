//go:build compose

package compose

import (
	"os"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/acceptance"
)

// TestComposePins resolves a caller-supplied Compose file and proves the
// tama, tama-mcp, and provider services reference the reported revisions.
// It does not start containers and is not live runtime acceptance.
func TestComposePins(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("TAMA_LINK_COMPOSE_FILE"))
	if path == "" {
		t.Fatal("TAMA_LINK_COMPOSE_FILE is not set; this command does not start containers and is not live acceptance")
	}
	if os.Getenv("TAMA_LINK_COMPOSE_START") == "1" {
		t.Fatal("compose startup is not executed by this command")
	}
	err := acceptance.CheckComposePins(path, acceptance.ComposeRevisions{
		TamaMCP:  os.Getenv("TAMA_LINK_COMPOSE_TAMAMCP_SHA"),
		Tama:     os.Getenv("TAMA_LINK_COMPOSE_TAMA_SHA"),
		Provider: os.Getenv("TAMA_LINK_COMPOSE_PROVIDER_SHA"),
	})
	if err != nil {
		t.Fatalf("compose pin check failed: %v\nthis command does not start containers and is not live acceptance", err)
	}
	t.Log("compose service revisions match; containers were not started; this is not live acceptance")
}
