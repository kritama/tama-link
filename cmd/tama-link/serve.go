package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/server"
	"github.com/kritama/tama-link/internal/version"
)

// serveConfig holds the validated serve command inputs.
type serveConfig struct {
	profileName profile.Name
	configDir   string
}

// runServe implements the serve command. Standard output is reserved for MCP
// JSON-RPC frames, so diagnostics go to stderr only.
func runServe(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	cfg, ok := parseServeFlags(args, stderr)
	if !ok {
		return 2
	}

	if err := cfg.resolveProfile(); err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}

	srv := server.New(version.Version)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		writef(stderr, "tama-link: MCP server failed: %v\n", err)
		return 1
	}
	return 0
}

func parseServeFlags(args []string, stderr io.Writer) (serveConfig, bool) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg serveConfig
	profileFlag := fs.String("profile", "", "profile name to serve (required)")
	fs.StringVar(&cfg.configDir, "config-dir", "", "override the Tama Link configuration directory")

	if err := fs.Parse(args); err != nil {
		return cfg, false
	}
	if *profileFlag == "" {
		writef(stderr, "tama-link: serve requires --profile\n")
		fs.Usage()
		return cfg, false
	}

	name, err := profile.ParseName(*profileFlag)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return cfg, false
	}
	cfg.profileName = name
	return cfg, true
}

// resolveProfile resolves the configuration directory and verifies the
// profile file exists. It returns a descriptive error on any failure.
func (c *serveConfig) resolveProfile() error {
	configDir, err := profile.ConfigDir(c.configDir)
	if err != nil {
		return err
	}
	c.configDir = configDir

	path := profile.Path(configDir, c.profileName)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("profile %q not found at %s", c.profileName, path)
	}
	if info.IsDir() {
		return fmt.Errorf("profile path %s is a directory", path)
	}
	return nil
}
