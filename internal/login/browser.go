package login

import (
	"os/exec"
	"time"
)

// browserLaunchBudget bounds how long the handoff waits for the opener's
// exit result. Openers hand the URL to the platform session and exit
// quickly, and a failure exits non-zero well inside the budget; an opener
// still running past it has dispatched the URL and stays attached to the
// launched application, which the handoff treats as success.
const browserLaunchBudget = 5 * time.Second

// DefaultOpenBrowser opens the authorization URL in the user's browser
// with the fixed platform opener and its bounded launch wait.
var DefaultOpenBrowser = openBrowser

// runBrowserCommand launches the opener and observes its exit. Start only
// proves the executable spawned, so the exit result decides the outcome:
// a non-zero exit is a launch failure that must fall back to the manual
// handoff. The wait for the exit is bounded by budget: an opener still
// running past it is not killed — desktop openers legitimately remain
// attached to the launched application — and the handoff counts as
// successful while the background wait keeps reaping the child until it
// exits.
func runBrowserCommand(cmd *exec.Cmd, budget time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return nil
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
