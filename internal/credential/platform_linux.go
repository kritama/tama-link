//go:build linux

package credential

import (
	"context"
	"errors"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential/secretservice"
)

// openPlatform opens the Secret Service collection behind the default alias.
// It does not create an application collection. interactive selects whether an
// existing locked collection may prompt; it does not change the collection.
func openPlatform(interactive bool) (keyring.Keyring, error) {
	backends := secureBackends()
	if serviceName == "" || len(backends) != 1 || backends[0] != keyring.SecretServiceBackend {
		return nil, errors.New("linux credential backend must be the secret service")
	}
	return secretservice.Open(context.Background(), secretservice.Config{Interactive: interactive})
}
