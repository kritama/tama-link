package login

import (
	"os/exec"
	"time"
)

// browserLaunchBudget bounds the time the opener process may run before the
// handoff is treated as failed. Openers hand the URL to the platform session
// and exit quickly; a process still alive after the budget either lost its
// session or is wedged, and the manual handoff applies either way. The
// variable exists so tests can shrink the budget.
var browserLaunchBudget = 5 * time.Second

// DefaultOpenBrowser opens the authorization URL in the user's browser
// with the fixed platform opener and its bounded launch wait.
var DefaultOpenBrowser = openBrowser

// runBrowserCommand launches the opener, observes its exit with a bounded
// wait, and reaps it. Start only proves the executable spawned; the exit
// result decides whether the user actually sees the authorization page, so
// an unsuccessful exit is a launch failure and a wedged opener is killed at
// the budget instead of outliving the callback deadline.
func runBrowserCommand(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(browserLaunchBudget):
		_ = cmd.Process.Kill()
		return <-done
	}
}

// handoff presents the authorization URL to the user. With an opener, the
// fixed platform browser opens it; a launch failure or a nil opener falls
// back to the manual handoff. The fallback never invalidates the attempt:
// the bounded callback wait continues unchanged.
func (s *Service) handoff(rawURL string) {
	if s.openBrowser != nil {
		if err := s.openBrowser(rawURL); err == nil {
			s.reporter.Note("waiting for browser authorization (up to %s)", s.callbackWait)
			return
		}
		s.reporter.Note("could not open a browser; open the authorization URL in your browser")
	}
	s.reporter.AuthorizationURL(rawURL)
	s.reporter.Note("waiting for the authorization callback (up to %s)", s.callbackWait)
}
