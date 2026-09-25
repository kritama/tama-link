//go:build windows

package login

import "os/exec"

// browserCommand is the fixed Windows opener through the native URL
// protocol handler: rundll32 delegates the URL to the user's registered
// browser with the URL as a single argument, never a shell invocation.
func browserCommand(rawURL string) *exec.Cmd {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
}

// openBrowser opens the authorization URL through the native Windows URL
// handler without a shell and without a user-controlled executable name.
// It observes the opener process with a bounded wait: a present opener
// that exits unsuccessfully reports an error so the caller can fall back
// to the manual handoff.
func openBrowser(rawURL string) error {
	return runBrowserCommand(browserCommand(rawURL))
}
