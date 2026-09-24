package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
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

// gcInterval bounds how long a retention deadline waits before the serve
// process applies it. The store's GC is one idempotent transaction, so
// overlapping sweeps across processes are safe.
const gcInterval = 5 * time.Minute

// runGC applies the store's retention deadlines for the serve process's
// lifetime: without it a continuously running or restarted process never
// expires terminal payloads, tombstones, and their idempotency rows. One
// sweep runs immediately, then on every tick; a failed sweep is retried on
// the next tick. The goroutine owns its ctx and returns when it is
// cancelled, so cleanup can join it before closing the store.
func runGC(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		// A failed sweep is retried on the next tick; the next deadline
		// still applies.
		_, _ = st.GC(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

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

	hooks := fixtureHooks()
	if hooks.open == nil {
		hooks.open = credential.New
	}
	app, cleanup, err := buildApp(ctx, p, cfg.configDir, hooks)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}
	defer cleanup()

	srv, err := server.New(p, version.Version, app)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		writef(stderr, "tama-link: MCP server failed: %v\n", err)
		return 1
	}
	return 0
}

// credentialOpener opens one profile's secure credential handle from its
// canonical namespace. Production passes credential.New; tests substitute a
// deterministic backend.
type credentialOpener func(namespace string) (*credential.Keyring, error)

// stateLayout resolves one profile's canonical durable state locations: the
// profile-scoped database path and the credential namespace. Two named
// profiles can never collapse onto one database or one credential space, no
// matter how their state references are configured.
func stateLayout(p *profile.Profile, configDir string) (dbPath string, namespace string, err error) {
	stateRoot, err := profile.StateDir(configDir)
	if err != nil {
		return "", "", err
	}
	dbPath, err = profile.DatabasePath(stateRoot, p)
	if err != nil {
		return "", "", err
	}
	return dbPath, profile.CredentialNamespace(p), nil
}

// buildApp wires one profile's full application stack: secure credential
// backend, encrypted state store, OAuth client, stateless upstream client,
// verified adapter, leased worker, and the application service. The cleanup
// callback shuts down the worker and closes the store.
func buildApp(ctx context.Context, p *profile.Profile, configDir string, hooks serveHooks) (server.App, func(), error) {
	dbPath, namespace, err := stateLayout(p, configDir)
	if err != nil {
		return nil, nil, err
	}
	if hooks.open == nil {
		hooks.open = credential.New
	}
	kr, err := hooks.open(namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("open credential backend: %w", err)
	}

	st, err := store.Open(ctx, dbPath, kr, store.Config{Limits: p.EffectiveLimits()})
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
		TokenProvider:      hooks.tokenProvider(oauthClient),
		MaxResponseBytes:   int64(limits.ResponseBytes),
		HTTPClient:         hooks.client(),
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
	connect = application.MemoConnect(connect)
	kind, err := p.Kind()
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("resolve profile kind: %w", err)
	}
	var workerService *worker.Service
	var taskService *application.TaskService
	switch kind {
	case profile.KindApp:
		taskService, err = application.NewTaskService(st, connect, application.TaskConfig{
			Owner:       fmt.Sprintf("tama-link/%d", os.Getpid()),
			LeaseTTL:    workerLeaseTTL,
			Credentials: oauthClient,
		})
		if err != nil {
			_ = st.Close()
			return nil, nil, fmt.Errorf("configure task runner: %w", err)
		}
	case profile.KindSystem:
		workerService, err = worker.NewService(st, application.NewExecutor(connect), worker.Config{
			Owner:    fmt.Sprintf("tama-link/%d", os.Getpid()),
			LeaseTTL: workerLeaseTTL,
		})
		if err != nil {
			_ = st.Close()
			return nil, nil, fmt.Errorf("configure worker: %w", err)
		}
	default:
		_ = st.Close()
		return nil, nil, fmt.Errorf("unknown profile kind %q", kind)
	}

	app, err := application.New(application.Config{
		Profile:          p,
		Store:            st,
		Connect:          connect,
		Worker:           workerService,
		Tasks:            taskService,
		AdapterVersion:   version.Version,
		CredentialsReady: hooks.credentialsReady(oauthClient),
	})
	if err != nil {
		if taskService != nil {
			taskService.Stop()
		}
		if workerService != nil {
			workerService.Stop()
		}
		_ = st.Close()
		return nil, nil, fmt.Errorf("configure application: %w", err)
	}

	// Startup recovery offers pending work for this profile's execution
	// model and returns immediately. The other model is not started.
	if workerService != nil {
		_ = workerService.Start(ctx)
	}
	if taskService != nil {
		_ = taskService.Start(ctx)
	}

	// Retention sweeps run for the process lifetime under an owned,
	// cancellable context: cleanup cancels before the store closes and
	// joins the sweep so no GC can run against a closed store.
	gcCtx, stopGC := context.WithCancel(context.Background())
	var gcWg sync.WaitGroup
	gcWg.Add(1)
	go func() {
		defer gcWg.Done()
		runGC(gcCtx, st)
	}()

	cleanup := func() {
		if taskService != nil {
			taskService.Stop()
		}
		if workerService != nil {
			workerService.Stop()
		}
		stopGC()
		gcWg.Wait()
		_ = st.Close()
	}
	return app, cleanup, nil
}

// serveHooks carries the process-start dependencies that a fixture build
// may replace. The zero value uses the production credential and token path.
type serveHooks struct {
	open       credentialOpener
	token      func(context.Context) (string, error)
	ready      func(context.Context) (bool, error)
	httpClient func() *http.Client
}

func (h serveHooks) tokenProvider(client *oauth.Client) func(context.Context) (string, error) {
	if h.token != nil {
		return h.token
	}
	return func(ctx context.Context) (string, error) {
		tok, err := client.Token(ctx)
		if err != nil {
			if errors.Is(err, oauth.ErrLeaseContention) {
				return "", fmt.Errorf("%w: %w", upstream.ErrTokenContended, err)
			}
			return "", err
		}
		return tok, nil
	}
}

func (h serveHooks) credentialsReady(client *oauth.Client) func(context.Context) (bool, error) {
	if h.ready != nil {
		return h.ready
	}
	return client.HasCredentials
}

func (h serveHooks) client() *http.Client {
	if h.httpClient != nil {
		return h.httpClient()
	}
	return nil
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
