// Command tama-link runs the local Tama Link MCP compatibility proxy.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/server"
	"github.com/kritama/tama-link/internal/version"
)

const usage = `Usage:
  tama-link serve
  tama-link version

The serve command starts the Tama Link MCP server over stdin/stdout.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		writeLine(stderr, usage)
		return 2
	}

	switch args[0] {
	case "serve":
		if err := server.New(version.Version).Run(ctx, &mcp.StdioTransport{}); err != nil {
			writef(stderr, "tama-link: MCP server failed: %v\n", err)
			return 1
		}

		return 0
	case "version":
		writeLine(stdout, version.String())
		return 0
	case "help", "-h", "--help":
		writeLine(stdout, usage)
		return 0
	default:
		writef(stderr, "tama-link: unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
}

func writeLine(writer io.Writer, values ...any) {
	_, _ = fmt.Fprintln(writer, values...)
}

func writef(writer io.Writer, format string, values ...any) {
	_, _ = fmt.Fprintf(writer, format, values...)
}
