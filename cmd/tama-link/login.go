package main

import (
	"flag"
	"io"

	"github.com/kritama/tama-link/internal/profile"
)

// runLogin implements the login command. Interactive OAuth authorization
// lands in Phase 3; this phase only reserves the command surface.
func runLogin(args []string, _ io.Writer, stderr io.Writer) int {
	if !requireProfileFlag("login", args, stderr) {
		return 2
	}
	writef(stderr, "tama-link: login is not implemented in this phase\n")
	return 1
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
