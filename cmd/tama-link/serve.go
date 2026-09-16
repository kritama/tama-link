package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/application"
	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/oauth"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/server"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/version"
	"github.com/kritama/tama-link/internal/worker"
)

// serveConfig holds the validated serve command inputs.
type serveConfig struct {
	profileName profile.Name
	configDir   string
}

// workerLeaseTTL bounds one local execution lease. It outlives any single
// synchronous execution (the per-request upstream timeout defaults to 60s);
// expiry is the crash-recovery hand-off to another process.
const workerLeaseTTL = 2 * time.Minute

// runServe implements the serve command. Standard output is reserved for MCP
// JSON-RPC frames, so diagnostics go to stderr only.
func runServe(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	cfg, ok := parseServeFlags(args, stderr)
	if !ok {
		return 2
	}

	p, err := profile.Load(cfg.profileName, cfg.configDir)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}

	app, cleanup, err := buildApp(ctx, p, cfg.configDir)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}
	defer cleanup()

	srv := server.New(p, version.Version, app)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		writef(stderr, "tama-link: MCP server failed: %v\n", err)
		return 1
	}
	return 0
}

// buildApp wires one profile's full application stack: secure credential
// backend, encrypted state store, OAuth client, stateless upstream client,
// verified adapter, leased worker, and the application service. The cleanup
// callback shuts down the worker and closes the store.
func buildApp(ctx context.Context, p *profile.Profile, configDir string) (server.App, func(), error) {
	kr, err := credential.New(string(p.Name))
	if err != nil {
		return nil, nil, fmt.Errorf("open credential backend: %w", err)
	}

	dir, err := profile.ConfigDir(configDir)
	if err != nil {
		return nil, nil, err
	}
	statePath := filepath.Join(dir, "state", p.State.Database+".db")
	st, err := store.Open(ctx, statePath, kr, store.Config{Limits: p.EffectiveLimits()})
	if err != nil {
		return nil, nil, fmt.Errorf("open state store: %w", err)
	}

	oauthClient, err := oauth.New(oauth.Config{
		Endpoint: p.Endpoint,
		Issuer:   p.Issuer,
		Secrets:  kr,
		Lease:    st,
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure oauth: %w", err)
	}

	limits := p.EffectiveLimits()
	up, err := upstream.New(upstream.Config{
		Endpoint: p.Endpoint,
		ClientInfo: mcp.Implementation{
			Name:    "tama-link",
			Version: version.Version,
		},
		ClientCapabilities: []byte(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      oauthClient.Token,
		MaxResponseBytes:   int64(limits.ResponseBytes),
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure upstream client: %w", err)
	}

	adapter, err := tama2026.New(tama2026.Config{
		Profile:        *p,
		Upstream:       up,
		AdapterVersion: version.Version,
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure adapter: %w", err)
	}

	connect := func(ctx context.Context) (*tama2026.Connection, error) {
		return adapter.Connect(ctx)
	}
	executor := application.NewExecutor(connect)
	workerService, err := worker.NewService(st, executor, worker.Config{
		Owner:    fmt.Sprintf("tama-link/%d", os.Getpid()),
		LeaseTTL: workerLeaseTTL,
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure worker: %w", err)
	}

	app, err := application.New(application.Config{
		Profile:        p,
		Store:          st,
		Connect:        connect,
		Worker:         workerService,
		AdapterVersion: version.Version,
	})
	if err != nil {
		workerService.Stop()
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure application: %w", err)
	}

	// Startup recovery re-runs pending replayable submissions whose leases
	// have expired. Failures surface per-submission on await; the serve
	// command still starts so already-accepted work stays inspectable.
	if err := workerService.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "tama-link: startup recovery incomplete: %v\n", err)
	}

	cleanup := func() {
		workerService.Stop()
		_ = st.Close()
	}
	return app, cleanup, nil
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
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: serve takes no positional arguments\n")
		fs.Usage()
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
