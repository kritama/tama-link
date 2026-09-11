package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// maxProfileFileBytes bounds one profile document on disk.
const maxProfileFileBytes = 1 << 20

// Load reads, decodes, and validates the named profile under configDir.
// The profile file must be a regular file; symlinks and special files are
// rejected so a profile cannot point at untrusted content.
func Load(name Name, configDir string) (*Profile, error) {
	dir, err := ConfigDir(configDir)
	if err != nil {
		return nil, err
	}

	path := Path(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile %q not found at %s", name, path)
		}
		return nil, fmt.Errorf("read profile %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("profile path %s is a directory", path)
	}
	if info.Mode()&os.ModeType != 0 {
		return nil, fmt.Errorf("profile path %s is not a regular file", path)
	}
	if info.Size() > maxProfileFileBytes {
		return nil, fmt.Errorf("profile %s exceeds %d bytes", path, maxProfileFileBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", path, err)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, fmt.Errorf("decode profile %s: %w", path, err)
	}

	var p Profile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode profile %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("profile %s contains trailing data", path)
	}

	if err := p.Validate(name); err != nil {
		return nil, fmt.Errorf("profile %q: %w", name, err)
	}
	return &p, nil
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
		_, err = dec.Token()
		return err
	}
	return walk()
}
