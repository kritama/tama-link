// Package credential is Tama Link's platform keyring wrapper. It is the only
// component that touches the platform credential store. It holds the
// profile-scoped state-encryption key (D4/D14) and, in a later phase, OAuth
// secrets. Nothing secret is ever written to the SQLite state store, to logs,
// or to JSON output.
package credential

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	keyring "github.com/99designs/keyring"
)

// ErrUnavailable reports that the secure credential backend is absent or
// failed. Callers must fail closed: Tama Link never falls back to a file, an
// environment variable, or plaintext (D14).
var ErrUnavailable = errors.New("credential backend unavailable")

// serviceName is the credential service name shared by every profile.
const serviceName = "Tama Link"

// Keyring is one profile's handle on the platform credential store. It
// implements store.KeyProvider for the state-encryption key. Every stored item
// is namespaced by profile so one profile can never read another's secrets.
type Keyring struct {
	kr     keyring.Keyring
	prefix string
}

// secureBackends lists the credential backends allowed on this OS. File and
// pass backends are deliberately excluded so headless Linux with no secure
// service fails closed instead of silently writing a plaintext file.
func secureBackends() []keyring.BackendType {
	switch runtime.GOOS {
	case "darwin":
		return []keyring.BackendType{keyring.KeychainBackend}
	case "windows":
		return []keyring.BackendType{keyring.WinCredBackend}
	default:
		return []keyring.BackendType{keyring.SecretServiceBackend}
	}
}

// probeTimeout bounds the startup availability probe. Backends that cannot
// complete a write within this window (for example a headless Secret Service
// waiting for user interaction) are treated as unavailable so Tama Link
// fails fast with a clear error instead of hanging.
var probeTimeout = 5 * time.Second

// New opens the secure credential backend for profile and namespaces it by
// profile. It fails closed with ErrUnavailable when no secure backend is
// available or cannot complete an availability probe.
func New(profile string) (*Keyring, error) {
	if profile == "" {
		return nil, errors.New("credential: profile name is required")
	}
	cfg := keyring.Config{
		ServiceName:     serviceName,
		AllowedBackends: secureBackends(),
	}
	kr, err := keyring.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if err := probeBackend(profile, kr); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return &Keyring{kr: kr, prefix: profile + "/"}, nil
}

// probeBackend verifies that the backend completes a Set/Get/Remove cycle
// on one disposable profile-scoped entry. Platform backends can accept a
// connection while still being unable to complete a write, so connect
// success alone is not availability.
func probeBackend(profile string, kr keyring.Keyring) error {
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		key := profile + "/__probe__"
		err := kr.Set(keyring.Item{
			Key:         key,
			Data:        []byte{1},
			Label:       "Tama Link availability probe",
			Description: "Temporary entry; safe to delete",
		})
		if err == nil {
			if _, err = kr.Get(key); err == nil {
				err = kr.Remove(key)
			}
		}
		done <- result{err: err}
	}()
	select {
	case res := <-done:
		return res.err
	case <-time.After(probeTimeout):
		return fmt.Errorf("backend did not complete an availability probe within %s", probeTimeout)
	}
}

// newWith builds a Keyring over an already-open backend. It is used by tests
// and by callers that have already resolved a backend.
func newWith(profile string, kr keyring.Keyring) *Keyring {
	return &Keyring{kr: kr, prefix: profile + "/"}
}
