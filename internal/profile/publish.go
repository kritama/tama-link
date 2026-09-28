package profile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrExists reports that a profile file already occupies the target name.
// Publication never replaces that file.
var ErrExists = errors.New("profile already exists")

// StagingDirName is the private directory under the configuration root that
// holds non-secret bootstrap journals. It is not a profile directory.
const StagingDirName = "bootstrap"

// Publish validates p and atomically creates its profile file without
// replacing an existing target. The published file is reloaded before
// success so a partial or unreadable document is not reported as ready.
func Publish(configDir string, p *Profile) error {
	if p == nil {
		return fmt.Errorf("profile is required")
	}
	if err := p.Validate(p.Name); err != nil {
		return fmt.Errorf("publish profile: %w", err)
	}
	root, err := ConfigDir(configDir)
	if err != nil {
		return err
	}
	if err := EnsurePrivateDir(root); err != nil {
		return fmt.Errorf("prepare configuration directory: %w", err)
	}
	profiles := filepath.Join(root, ProfilesDirName)
	if err := EnsurePrivateDir(profiles); err != nil {
		return fmt.Errorf("prepare profile directory: %w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode profile: %w", err)
	}
	data = append(data, '\n')
	target := p.Name.String() + ProfileFileSuffix
	if err := WritePrivateFile(profiles, target, data, false); err != nil {
		return err
	}
	if _, err := Load(p.Name, configDir); err != nil {
		return fmt.Errorf("reload published profile: %w", err)
	}
	return nil
}

// EnsurePrivateDir creates the last path component as a private directory
// owned by the current user, without following a link at that name. The
// parent must already exist, be owned by the current user, and must not be
// group- or world-writable.
func EnsurePrivateDir(path string) error {
	parent, name := filepath.Split(filepath.Clean(path))
	parent = strings.TrimRight(parent, string(filepath.Separator))
	if parent == "" || name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid private directory %q", path)
	}
	return ensurePrivateChild(parent, name)
}

func validateStagingName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid file name %q", name)
	}
	return nil
}

func randomStagingName(name string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("staging name: %w", err)
	}
	return "." + name + "." + hex.EncodeToString(buf[:]) + ".tmp", nil
}
