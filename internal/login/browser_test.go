package login

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHandoffBrowserPreferred proves the browser opener runs first: a
// successful launch notes the wait and never prints the URL to stdout.
func TestHandoffBrowserPreferred(t *testing.T) {
	t.Parallel()

	var opened string
	var notes []string
	var urls []string
	svc := &Service{
		openBrowser: func(rawURL string) error {
			opened = rawURL
			return nil
		},
		reporter: recordingReporter{notes: &notes, urls: &urls},
	}
	svc.handoff("https://issuer.example/oauth/authorize?state=s1")
	if opened != "https://issuer.example/oauth/authorize?state=s1" {
		t.Fatalf("opened = %q", opened)
	}
	if len(urls) != 0 {
		t.Fatalf("stdout urls = %v, want none when the browser opened", urls)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "browser authorization") {
		t.Fatalf("notes = %v", notes)
	}
}

// TestHandoffBrowserFailureFallsBack proves a launch failure degrades to
// the manual handoff: the URL goes to stdout and the wait is announced.
func TestHandoffBrowserFailureFallsBack(t *testing.T) {
	t.Parallel()

	var notes []string
	var urls []string
	svc := &Service{
		openBrowser: func(string) error { return errOpenFailed },
		reporter:    recordingReporter{notes: &notes, urls: &urls},
	}
	svc.handoff("https://issuer.example/oauth/authorize?state=s1")
	if len(urls) != 1 || urls[0] != "https://issuer.example/oauth/authorize?state=s1" {
		t.Fatalf("urls = %v, want exactly the authorization URL", urls)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "could not open a browser") {
		t.Fatalf("notes = %v", notes)
	}
}

// TestHandoffNoBrowser proves --no-browser skips the opener entirely and
// goes straight to the manual handoff.
func TestHandoffNoBrowser(t *testing.T) {
	t.Parallel()

	var notes []string
	var urls []string
	svc := &Service{
		openBrowser: nil,
		reporter:    recordingReporter{notes: &notes, urls: &urls},
	}
	svc.handoff("https://issuer.example/oauth/authorize?state=s1")
	if len(urls) != 1 {
		t.Fatalf("urls = %v, want the authorization URL", urls)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "waiting for the authorization callback") {
		t.Fatalf("notes = %v", notes)
	}
}

// recordingReporter records the reporter's two message kinds for
// assertions.
type recordingReporter struct {
	notes *[]string
	urls  *[]string
}

func (r recordingReporter) AuthorizationURL(rawURL string) {
	*r.urls = append(*r.urls, rawURL)
}

func (r recordingReporter) Note(format string, _ ...any) {
	*r.notes = append(*r.notes, format)
}

var errOpenFailed = errors.New("browser unavailable")

// shellFor returns a one-shot shell invocation for tests; the platform
// adapters themselves never use a shell.
func shellFor(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", script)
	}
	return exec.Command("/bin/sh", "-c", script)
}

// TestRunBrowserCommandReportsOpenerExit proves the opener's exit result
// decides the launch outcome under a bounded wait: a nonzero exit is a
// launch failure the handoff must turn into the manual authorization URL,
// a clean exit succeeds, and an opener still attached to the launched
// application past the budget counts as a successful handoff without
// blocking the attempt or killing the child.
func TestRunBrowserCommandReportsOpenerExit(t *testing.T) {
	t.Parallel()

	if err := runBrowserCommand(shellFor(t, "exit 3"), browserLaunchBudget); err == nil {
		t.Fatal("a nonzero opener exit must be a launch failure")
	}
	if err := runBrowserCommand(shellFor(t, "exit 0"), browserLaunchBudget); err != nil {
		t.Fatalf("exit 0: %v", err)
	}

	start := time.Now()
	if err := runBrowserCommand(shellFor(t, "sleep 30"), 100*time.Millisecond); err != nil {
		t.Fatalf("an opener still attached to the launched app past the budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("bounded wait took %s, want well under the callback deadline", elapsed)
	}
}
