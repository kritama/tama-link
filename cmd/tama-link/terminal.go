package main

// stdinIsInteractive reports whether login may prompt. Tests replace it so
// a developer terminal cannot make non-interactive command tests wait.
var stdinIsInteractive = detectStdinInteractive

func stdinInteractive() bool { return stdinIsInteractive() }
