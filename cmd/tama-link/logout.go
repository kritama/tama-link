package main

import "io"

// runLogout implements the logout command. Credential removal and revocation
// land in Phase 3; this phase only reserves the command surface.
func runLogout(args []string, _ io.Writer, stderr io.Writer) int {
	if !requireProfileFlag("logout", args, stderr) {
		return 2
	}
	writef(stderr, "tama-link: logout is not implemented in this phase\n")
	return 1
}
