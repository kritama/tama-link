// Command tama-link runs the local Tama Link MCP compatibility proxy.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

const usage = `Usage:
  tama-link serve --profile <name> [--config-dir <dir>]
  tama-link login --profile <name>
  tama-link logout --profile <name>
  tama-link version [--json]

The serve command starts the Tama Link MCP server over stdin/stdout for the
selected profile. Standard output is reserved for MCP JSON-RPC frames.
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
		return runLogin(rest, stdout, stderr)
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

// writef writes a formatted string to w, ignoring write failures.
func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
