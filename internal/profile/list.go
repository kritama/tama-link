package profile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one profile file discovered under the configuration directory.
// Invalid or unreadable files are reported and cannot be selected.
type Entry struct {
	Name    Name
	Profile *Profile
	Problem string
}

// Valid reports whether the entry can be logged in.
func (e Entry) Valid() bool { return e.Profile != nil && e.Problem == "" }

// ListPrivateNames reads names from dir through a pinned directory handle.
// The directory must be owned by the current user and private. A missing
// directory is an empty listing. Symlinks are not followed.
func ListPrivateNames(dir string) ([]string, error) {
	return listProfileNames(dir)
}

// List reads the profile directory without following links. A missing
// directory is an empty listing, not an error. Names are sorted.
func List(configDir string) ([]Entry, error) {
	root, err := ConfigDir(configDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, ProfilesDirName)
	names, err := listProfileNames(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(names))
	for _, fileName := range names {
		if !strings.HasSuffix(fileName, ProfileFileSuffix) {
			continue
		}
		bare := strings.TrimSuffix(fileName, ProfileFileSuffix)
		name, err := ParseName(bare)
		if err != nil {
			entries = append(entries, Entry{Problem: fmt.Sprintf("skipped %s: invalid profile name", fileName)})
			continue
		}
		loaded, err := Load(name, configDir)
		if err != nil {
			entries = append(entries, Entry{Name: name, Problem: err.Error()})
			continue
		}
		entries = append(entries, Entry{Name: name, Profile: loaded})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}
