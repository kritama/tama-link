//go:build darwin

package login

import "testing"

// TestBrowserCommandIsShellFree proves the macOS opener is the fixed
// platform `open` executable with the URL as a single argument, never a
// shell invocation.
func TestBrowserCommandIsShellFree(t *testing.T) {
	t.Parallel()

	const wantURL = "https://issuer.example/oauth/authorize?state=s1&x=$(rm -rf)"
	cmd := browserCommand(wantURL)
	if len(cmd.Args) != 2 {
		t.Fatalf("args = %v, want open, url", cmd.Args)
	}
	if cmd.Args[0] != "open" {
		t.Fatalf("executable = %q, want open", cmd.Args[0])
	}
	if cmd.Args[1] != wantURL {
		t.Fatalf("url arg = %q, want the exact URL as a single argument", cmd.Args[1])
	}
	if cmd.Stderr != nil || cmd.Stdout != nil {
		t.Fatal("opener must not inherit the process pipes")
	}
}
