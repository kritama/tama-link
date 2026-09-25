package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kritama/tama-link/internal/bootstrap"
	"github.com/kritama/tama-link/internal/login"
	"github.com/kritama/tama-link/internal/profile"
)

// loginFlags holds parsed login inputs and which flags the user supplied.
type loginFlags struct {
	profile      string
	profileSet   bool
	address      string
	addressSet   bool
	kind         string
	kindSet      bool
	issuer       string
	issuerSet    bool
	configDir    string
	configDirSet bool
	noBrowser    bool
	yes          bool
}

// runLogin implements the login command. Existing profiles keep the current
// authorization path. A missing profile can be created only through the
// explicit bootstrap flow. Standard output stays reserved for the manual
// authorization URL.
func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, ok := parseLoginFlags(args, stderr)
	if !ok {
		return 2
	}
	svc, err := bootstrap.New(bootstrap.Options{
		ConfigDir:    flags.configDir,
		ConfigDirSet: flags.configDirSet,
		Interactive:  stdinInteractive(),
		Stdin:        os.Stdin,
		Stdout:       stdout,
		Stderr:       stderr,
		HTTP:         fixtureHooks().client(),
		OpenSession: func(ctx context.Context, shell *profile.Profile) (bootstrap.Session, error) {
			return openBootstrapSession(ctx, shell, flags.configDir, stdout, stderr)
		},
		ReadCatalog: readBootstrapCatalog,
		LoginExisting: func(ctx context.Context, p *profile.Profile) error {
			return runExistingLogin(ctx, p, flags, stdout, stderr)
		},
	})
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		return 1
	}
	result, err := svc.Run(ctx, bootstrap.Request{
		Address:    flags.address,
		AddressSet: flags.addressSet,
		Type:       flags.kind,
		TypeSet:    flags.kindSet,
		Profile:    flags.profile,
		ProfileSet: flags.profileSet,
		Issuer:     flags.issuer,
		IssuerSet:  flags.issuerSet,
		Yes:        flags.yes,
		NoBrowser:  flags.noBrowser,
	})
	if err != nil {
		writef(stderr, "tama-link: %v\n", err)
		if exit, ok := loginExit(err); ok {
			return exit.Status
		}
		return 1
	}
	if result.Created {
		writef(stderr, "tama-link: created profile %q\n", result.Name)
		writef(stderr, "tama-link: next: %s\n", result.ServeCommand)
		return 0
	}
	writef(stderr, "tama-link: logged in profile %q\n", result.Name)
	return 0
}

func runExistingLogin(ctx context.Context, p *profile.Profile, flags loginFlags, stdout, stderr io.Writer) error {
	if err := login.CheckProfile(p); err != nil {
		return &bootstrap.ExitError{Status: 2, Err: err}
	}
	svc, cleanup, err := buildLogin(ctx, p, loginConfig{
		profileName: p.Name,
		configDir:   flags.configDir,
		noBrowser:   flags.noBrowser,
	}, stdout, stderr)
	if err != nil {
		status := 1
		if errors.Is(err, login.ErrProfileOutdated) {
			status = 2
		}
		return &bootstrap.ExitError{Status: status, Err: err}
	}
	defer cleanup()
	if err := svc.Run(ctx); err != nil {
		return &bootstrap.ExitError{Status: 1, Err: err}
	}
	return nil
}

func parseLoginFlags(args []string, stderr io.Writer) (loginFlags, bool) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg loginFlags
	fs.StringVar(&cfg.profile, "profile", "", "profile name to log in or create")
	fs.StringVar(&cfg.address, "address", "", "path-free https origin of the Tama instance")
	fs.StringVar(&cfg.kind, "type", "", "profile type: app or system (default app)")
	fs.StringVar(&cfg.issuer, "issuer", "", "advertised authorization server to select")
	fs.StringVar(&cfg.configDir, "config-dir", "", "override the Tama Link configuration directory")
	fs.BoolVar(&cfg.noBrowser, "no-browser", false, "print the authorization URL and wait for the callback without opening a browser")
	fs.BoolVar(&cfg.yes, "yes", false, "skip the final summary confirmation")
	if err := fs.Parse(args); err != nil {
		return cfg, false
	}
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: login takes no positional arguments\n")
		fs.Usage()
		return cfg, false
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "profile":
			cfg.profileSet = true
		case "address":
			cfg.addressSet = true
		case "type":
			cfg.kindSet = true
		case "issuer":
			cfg.issuerSet = true
		case "config-dir":
			cfg.configDirSet = true
		}
	})
	if cfg.profileSet {
		if _, err := profile.ParseName(cfg.profile); err != nil {
			writef(stderr, "tama-link: %v\n", err)
			return cfg, false
		}
	}
	return cfg, true
}

// loginConfig remains the existing-profile runtime input.
type loginConfig struct {
	profileName profile.Name
	configDir   string
	noBrowser   bool
}

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

func loginOpenBrowser(noBrowser bool) func(string) error {
	if noBrowser {
		return nil
	}
	return login.DefaultOpenBrowser
}

type loginReporter struct {
	stdout io.Writer
	stderr io.Writer
}

func (r loginReporter) AuthorizationURL(rawURL string) {
	_, _ = fmt.Fprintln(r.stdout, rawURL)
}

func (r loginReporter) Note(format string, args ...any) {
	_, _ = fmt.Fprintf(r.stderr, "tama-link: "+format+"\n", args...)
}
