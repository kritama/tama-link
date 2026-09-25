//go:build windows

package login

import "testing"

// TestBrowserCommandIsShellFree proves the Windows opener is the native URL
// protocol handler through rundll32 — the URL delegated to the user's
// registered browser — never the Control Panel loader and never a shell
// invocation.
func TestBrowserCommandIsShellFree(t *testing.T) {
	t.Parallel()

	const wantURL = "https://issuer.example/oauth/authorize?state=s1&x=$(rm -rf)"
	cmd := browserCommand(wantURL)
	if len(cmd.Args) != 3 {
		t.Fatalf("args = %v, want rundll32, handler, url", cmd.Args)
	}
	if cmd.Args[0] != "rundll32" {
		t.Fatalf("executable = %q, want rundll32", cmd.Args[0])
	}
	if cmd.Args[1] != "url.dll,FileProtocolHandler" {
		t.Fatalf("handler = %q, want the URL protocol handler", cmd.Args[1])
	}
	if cmd.Args[2] != wantURL {
		t.Fatalf("url arg = %q, want the exact URL as a single argument", cmd.Args[2])
	}
	if cmd.Stderr != nil || cmd.Stdout != nil {
		t.Fatal("opener must not inherit the process pipes")
	}
}
