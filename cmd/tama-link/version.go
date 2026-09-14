package main

import (
	"encoding/json"
	"flag"
	"io"

	"github.com/kritama/tama-link/internal/version"
)

// versionOutput is the stable machine-readable version document.
type versionOutput struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
}

// runVersion implements the version command.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit a machine-readable JSON document")

	if err := fs.Parse(args); err != nil {
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		writef(stderr, "tama-link: version takes no positional arguments\n")
		fs.Usage()
		return 2
	}

	if !*asJSON {
		writef(stdout, "%s\n", version.String())
		return 0
	}

	enc := json.NewEncoder(stdout)
	if err := enc.Encode(versionOutput{
		Name:    "tama-link",
		Version: version.Version,
		Commit:  version.Commit,
		Built:   version.BuildDate,
	}); err != nil {
		return 1
	}
	return 0
}
