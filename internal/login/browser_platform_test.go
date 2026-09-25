//go:build !windows

package login

import "testing"

// TestBrowserCommandIsShellFree proves the Linux opener is a fixed
// executable with the URL as a single argument, never a shell invocation.
func TestBrowserCommandIsShellFree(t *testing.T) {
	t.Parallel()

	cmd := browserCommand("https://issuer.example/oauth/authorize?state=s1&x=$(rm -rf)")
	if cmd.Path == "" || cmd.Args[0] != "xdg-open" {
		t.Fatalf("command = %v", cmd.Path)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != "https://issuer.example/oauth/authorize?state=s1&x=$(rm -rf)" {
		t.Fatalf("args = %v, want the exact URL as a single argument", cmd.Args)
	}
	if cmd.Stderr != nil || cmd.Stdout != nil {
		t.Fatal("opener must not inherit the process pipes")
	}
}
