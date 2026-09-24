//go:build tamalinkfixture

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"sync"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential"
)

func init() {
	if os.Getenv("TAMA_LINK_FIXTURE") != "1" {
		return
	}
	openServeCredentials = func(namespace string) (*credential.Keyring, error) {
		return credential.NewWithBackend(namespace, newProcessKeyring()), nil
	}
	tokenFromFixture = func(context.Context) (string, error) { return "fixture-token", nil }
	credentialsReadyFromFixture = func(context.Context) (bool, error) { return true, nil }
	if path := os.Getenv("TAMA_LINK_FIXTURE_CA"); path != "" {
		httpClientFromFixture = func() *http.Client { return httpClientTrusting(path) }
	}
}

func httpClientTrusting(path string) *http.Client {
	pem, err := os.ReadFile(path)
	if err != nil {
		return &http.Client{}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return &http.Client{}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

type processKeyring struct {
	mu    sync.Mutex
	items map[string]keyring.Item
}

func newProcessKeyring() *processKeyring {
	return &processKeyring{items: map[string]keyring.Item{}}
}

func (m *processKeyring) Get(key string) (keyring.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return item, nil
}

func (m *processKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (m *processKeyring) Set(item keyring.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.Key] = item
	return nil
}

func (m *processKeyring) Remove(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *processKeyring) Keys() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.items))
	for key := range m.items {
		keys = append(keys, key)
	}
	return keys, nil
}
