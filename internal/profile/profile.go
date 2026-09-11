// Package profile resolves Tama Link profile names and file locations.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

const (
	// ConfigDirName is the Tama Link directory inside the platform user
	// configuration and state directories.
	ConfigDirName = "tama-link"

	// ProfilesDirName is the directory inside the configuration directory that
	// holds one JSON document per profile.
	ProfilesDirName = "profiles"

	// ProfileFileSuffix is appended to a profile name for its file.
	ProfileFileSuffix = ".json"

	// MaxNameLength bounds the portable profile name grammar.
	MaxNameLength = 64
)

// namePattern is the portable profile identifier grammar: 1-64 characters,
// starting with a lowercase letter, ending in a lowercase letter or digit,
// with lowercase letters, digits, or hyphens in between.
var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Name is a validated Tama Link profile name.
type Name string

// String returns the bare profile name.
func (n Name) String() string { return string(n) }

// ParseName validates a profile name against the portable identifier grammar.
func ParseName(value string) (Name, error) {
	if !namePattern.MatchString(value) {
		return "", fmt.Errorf(
			"invalid profile name %q: use 1-%d characters, start with a lowercase letter, and use only lowercase letters, digits, or hyphens",
			value, MaxNameLength,
		)
	}
	return Name(value), nil
}

// ConfigDir returns the Tama Link configuration directory. A non-empty
// override is returned as-is for tests and managed installations.
func ConfigDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	return filepath.Join(base, ConfigDirName), nil
}

// StateDir returns the Tama Link state directory. A non-empty override is
// returned as-is for tests and managed installations.
func StateDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := userStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve user state directory: %w", err)
	}
	return filepath.Join(base, ConfigDirName), nil
}

// userStateDir resolves the platform user state directory. The standard
// library has no helper for the XDG state directory, so it is derived here.
func userStateDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set")
		}
		return local, nil
	case "darwin":
		// macOS has no separate state directory convention.
		return os.UserConfigDir()
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return dir, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state"), nil
	}
}

// Path returns the profile file path for name under configDir.
func Path(configDir string, name Name) string {
	return filepath.Join(configDir, ProfilesDirName, name.String()+ProfileFileSuffix)
}
