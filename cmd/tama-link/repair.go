package main

import (
	"context"
	"flag"
	"io"
	"runtime"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/repair"
)

type repairConfig struct {
	profileName profile.Name
	configDir   string
}

// dialRepairService opens the Secret Service for an explicit repair. Tests
// replace it; serve never calls it.
var dialRepairService = func(ctx context.Context) (secretservice.Service, error) {
	return secretservice.Dial(ctx, secretservice.Config{Interactive: true})
}

func runRepair(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// Diagnostics stay on stderr. stdout is part of the command signature so
	// repair cannot accidentally claim the MCP stream.
	_ = stdout
	cfg, ok := parseRepairFlags(args, stderr)
	if !ok {
		return 2
	}
	if message := repairUnsupported(runtime.GOOS); message != "" {
		writef(stderr, "tama-link: %s\n", message)
		return 1
	}
	result, err := repairProfile(ctx, cfg)
	if err != nil {
		writef(stderr, "tama-link: repair failed: %v\n", err)
		return 1
	}
	writef(stderr, "tama-link: repaired profile %q: copied %d credential item(s), %d already in the default keyring, state key %s verified\n",
		cfg.profileName, result.Copied, result.AlreadyPresent, result.StateKeyID)
	return 0
}

func repairProfile(ctx context.Context, cfg repairConfig) (repair.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, repair.InteractionBudget)
	defer cancel()
	p, err := profile.Load(cfg.profileName, cfg.configDir)
	if err != nil {
		return repair.Result{}, err
	}
	dbPath, namespace, err := stateLayout(p, cfg.configDir)
	if err != nil {
		return repair.Result{}, err
	}
	service, err := dialRepairService(ctx)
	if err != nil {
		return repair.Result{}, err
	}
	result, err := repair.MigrateLegacyKeyring(ctx, service, repair.Request{
		DatabasePath: dbPath,
		Namespace:    namespace,
		Limits:       p.EffectiveLimits(),
		Interactive:  true,
	})
	closeErr := closeRepairService(service)
	if err != nil {
		return repair.Result{}, err
	}
	return result, closeErr
}

func closeRepairService(service secretservice.Service) error {
	closer, ok := service.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

func repairUnsupported(goos string) string {
	if goos == "linux" {
		return ""
	}
	return "repair migrates Linux Secret Service collections and is not used on " + goos
}

func parseRepairFlags(args []string, stderr io.Writer) (repairConfig, bool) {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg repairConfig
	profileFlag := fs.String("profile", "", "profile name (required)")
	fs.StringVar(&cfg.configDir, "config-dir", "", "override the Tama Link configuration directory")
	if err := fs.Parse(args); err != nil {
		return cfg, false
	}
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: repair takes no positional arguments\n")
		fs.Usage()
		return cfg, false
	}
	if *profileFlag == "" {
		writef(stderr, "tama-link: repair requires --profile\n")
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
