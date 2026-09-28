//go:build !linux

package credential

import "github.com/99designs/keyring"

// openPlatform opens the OS secure backend. macOS Keychain and Windows
// Credential Manager behavior is unchanged; the interactive flag only affects
// the Linux Secret Service adapter.
func openPlatform(bool) (keyring.Keyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName:     serviceName,
		AllowedBackends: secureBackends(),
	})
}
