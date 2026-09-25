package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/kritama/tama-link/internal/login"
	"github.com/kritama/tama-link/internal/profile"
)

// loginConfig holds the validated login command inputs.
type loginConfig struct {
	profileName profile.Name
	configDir   string
	noBrowser   bool
}

// runLogin implements the login command. The command stays thin: flag
// syntax, the profile contract, the durable runtime open, and the exit
// mapping. The interactive orchestration lives in internal/login. Exit
// status is stable: 0 for a committed and verified login, 1 for runtime
// and authorization failures, 2 for usage and profile-contract errors.
// Standard output is reserved for the manual authorization URL; everything
// else, including errors, goes to standard error.
func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, ok := parseLoginFlags(args, stderr)
	if !ok {
		return 2
	}
	p, err := profile.Load(cfg.profileName, cfg.configDir)
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}
	// The profile contract is a usage failure: check it before opening the
	// durable runtime, so an outdated profile never probes the keyring.
	if err := login.CheckProfile(p); err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 2
	}
	svc, cleanup, err := buildLogin(ctx, p, cfg, stdout, stderr)
	if err != nil {
		if errors.Is(err, login.ErrProfileOutdated) {
			writef(stderr, "tama-link: %v\n", err)
			return 2
		}
		writef(stderr, "tama-link: %v\n", err)
		return 1
	}
	defer cleanup()
	if err := svc.Run(ctx); err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 1
	}
	writef(stderr, "tama-link: logged in profile %q\n", p.Name)
	return 0
}

// buildLogin opens the profile's durable runtime and assembles the login
// service on top of it. The cleanup callback closes the state store; call
// it once the attempt finishes, on every exit path.
func buildLogin(ctx context.Context, p *profile.Profile, cfg loginConfig, stdout, stderr io.Writer) (*login.Service, func(), error) {
	rt, err := openProfileRuntime(ctx, p, cfg.configDir, fixtureHooks())
	if err != nil {
		return nil, nil, err
	}
	svc, err := login.New(login.Options{
		Profile:     p,
		Client:      rt.OAuth,
		Lease:       rt.Store,
		OpenBrowser: loginOpenBrowser(cfg.noBrowser),
		Reporter:    loginReporter{stdout: stdout, stderr: stderr},
	})
	if err != nil {
		rt.Close()
		return nil, nil, err
	}
	return svc, rt.Close, nil
}

// loginOpenBrowser selects the browser opener: nil chooses the manual
// handoff directly for --no-browser.
func loginOpenBrowser(noBrowser bool) func(string) error {
	if noBrowser {
		return nil
	}
	return login.DefaultOpenBrowser
}

// loginReporter maps the login service's two messages to the command's
// output channels: progress notes to standard error, the manual
// authorization URL to standard output.
type loginReporter struct {
	stdout io.Writer
	stderr io.Writer
}

func (r loginReporter) AuthorizationURL(rawURL string) {
	// stdout is reserved for this one intentional URL; it is never logged.
	_, _ = fmt.Fprintln(r.stdout, rawURL)
}

func (r loginReporter) Note(format string, args ...any) {
	_, _ = fmt.Fprintf(r.stderr, "tama-link: "+format+"\n", args...)
}

func parseLoginFlags(args []string, stderr io.Writer) (loginConfig, bool) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg loginConfig
	profileFlag := fs.String("profile", "", "profile name to log in (required)")
	fs.StringVar(&cfg.configDir, "config-dir", "", "override the Tama Link configuration directory")
	fs.BoolVar(&cfg.noBrowser, "no-browser", false, "print the authorization URL and wait for the callback without opening a browser")

	if err := fs.Parse(args); err != nil {
		return cfg, false
	}
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: login takes no positional arguments\n")
		fs.Usage()
		return cfg, false
	}
	if *profileFlag == "" {
		writef(stderr, "tama-link: login requires --profile\n")
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
