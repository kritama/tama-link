//go:build darwin

package login

import (
	"fmt"
	"os/exec"
)

// browserCommand builds the fixed macOS browser-opening command. It runs a
// known executable with the URL as a single argument and never evaluates a
// shell.
func browserCommand(rawURL string) *exec.Cmd {
	return exec.Command("open", rawURL)
}

// DefaultOpenBrowser opens the authorization URL with the fixed platform
// browser opener. It starts the opener and does not wait for it: the
// opener exits as soon as the browser is launched, and the child is reaped
// when the login process exits.
func DefaultOpenBrowser(rawURL string) error {
	cmd := browserCommand(rawURL)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch browser: %w", err)
	}
	return nil
}
