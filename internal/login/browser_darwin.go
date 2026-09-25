//go:build darwin

package login

import "os/exec"

// browserCommand is the fixed macOS opener: the URL is one argument, never
// a shell invocation.
func browserCommand(rawURL string) *exec.Cmd {
	return exec.Command("open", rawURL)
}

// openBrowser opens the authorization URL in the platform's default
// browser without a shell and without a user-controlled executable name.
// It observes the opener process with a bounded wait: a present opener
// that exits unsuccessfully reports an error so the caller can fall back
// to the manual handoff.
func openBrowser(rawURL string) error {
	return runBrowserCommand(browserCommand(rawURL), browserLaunchBudget)
}
