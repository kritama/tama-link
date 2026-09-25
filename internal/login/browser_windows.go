//go:build windows

package login

import "os/exec"

// browserCommand is the fixed Windows opener through the native URL
// handler: the URL is one argument, never a shell invocation.
func browserCommand(rawURL string) *exec.Cmd {
	return exec.Command("rundll32", "shell32.dll,Control_RunDLL", rawURL)
}

// openBrowser opens the authorization URL through the native Windows URL
// handler without a shell and without a user-controlled executable name.
// It observes the opener process with a bounded wait: a present opener
// that exits unsuccessfully reports an error so the caller can fall back
// to the manual handoff.
func openBrowser(rawURL string) error {
	return runBrowserCommand(browserCommand(rawURL))
}
