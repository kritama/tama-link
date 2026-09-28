// Command tama-link runs the local Tama Link MCP compatibility proxy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/kritama/tama-link/internal/profile"
)

const usage = `Usage:
  tama-link serve --profile <name> [--config-dir <dir>]
  tama-link login [--address <https-origin>] [--type <app|system>] [--profile <name>] [--issuer <https-url>] [--config-dir <dir>] [--no-browser] [--yes]
  tama-link logout --profile <name>
  tama-link version [--json]

The serve command starts the Tama Link MCP server over stdin/stdout for the
selected profile. Standard output is reserved for MCP JSON-RPC frames.

The login command authorizes an existing profile or, on an interactive
terminal, creates a new app or system profile from a reviewed template.
With --no-browser, or when the browser cannot be opened, it prints the
authorization URL and waits for the callback; standard output is reserved
for that URL. A non-interactive new profile requires --address.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		write(stderr, usage)
		return 2
	}

	command, rest := args[0], args[1:]
	switch command {
	case "serve":
		return runServe(ctx, rest, stdout, stderr)
	case "login":
		return runLogin(ctx, rest, stdout, stderr)
	case "logout":
		return runLogout(rest, stdout, stderr)
	case "version":
		return runVersion(rest, stdout, stderr)
	case "help", "-h", "--help":
		if len(rest) > 0 {
			writef(stderr, "tama-link: help takes no arguments\n")
			return 2
		}
		write(stdout, usage)
		return 0
	default:
		writef(stderr, "tama-link: unknown command %q\n\n%s", command, usage)
		return 2
	}
}

// write writes s to w, ignoring write failures. Diagnostics cannot
// meaningfully recover from an unwritable stream.
func write(w io.Writer, s string) {
	_, _ = io.WriteString(w, s)
}

// requireProfileFlag parses a command that takes exactly one required
// --profile flag and validates the name. It reports usage errors on stderr.
func requireProfileFlag(command string, args []string, stderr io.Writer) bool {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "profile name (required)")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		return false
	}
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: %s takes no positional arguments\n", command)
		fs.Usage()
		return false
	}
	if *profileFlag == "" {
		writef(stderr, "tama-link: %s requires --profile\n", command)
		fs.Usage()
		return false
	}
	if _, err := profile.ParseName(*profileFlag); err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return false
	}
	return true
}

// writef writes a formatted string to w, ignoring write failures.
func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
