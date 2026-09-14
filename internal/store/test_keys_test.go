package store

import "crypto/rand"

type testKeys struct {
	key []byte
}

func (m *testKeys) GetStateKey(string) ([]byte, bool, error) {
	return m.key, len(m.key) != 0, nil
}

func (m *testKeys) CreateStateKey() (string, []byte, error) {
	m.key = make([]byte, stateKeySize)
	if _, err := rand.Read(m.key); err != nil {
		return "", nil, err
	}
	return "state", m.key, nil
}
