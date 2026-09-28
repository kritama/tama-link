package repair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/store"
)

// validatePlan proves the selected state key decrypts existing ciphertext and
// that required credential records are readable before any destination write.
func validatePlan(ctx context.Context, req Request, identity store.PlaintextIdentity, plan []secretservice.ItemPlan) ([]byte, error) {
	stateKey := req.Namespace + "/state/" + identity.StateKeyID
	var state []byte
	required := map[string]struct{}{stateKey: {}}
	for _, slot := range identity.LiveSlots {
		required[req.Namespace+"/secret/"+slot] = struct{}{}
	}
	for _, item := range plan {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := credentialRecord(item.Value)
		if err != nil {
			return nil, fmt.Errorf("credential record %q is malformed", item.Key)
		}
		if _, must := required[item.Key]; must && len(record.Data) == 0 {
			return nil, fmt.Errorf("%w: item %q is empty", secretservice.ErrLegacyMissing, item.Key)
		}
		if item.Key == stateKey {
			state = record.Data
		}
		if _, must := required[item.Key]; must && item.Key != stateKey && !json.Valid(record.Data) {
			return nil, fmt.Errorf("credential record %q is malformed", item.Key)
		}
	}
	if len(state) != 32 {
		return nil, fmt.Errorf("%w: state key %q has invalid length", secretservice.ErrLegacyMissing, identity.StateKeyID)
	}
	if err := proveStateKey(ctx, req, identity.StateKeyID, state); err != nil {
		return nil, err
	}
	return bytes.Clone(state), nil
}

func credentialRecord(blob []byte) (keyring.Item, error) {
	var item keyring.Item
	if err := json.Unmarshal(blob, &item); err != nil {
		return keyring.Item{}, err
	}
	return item, nil
}

func proveStateKey(ctx context.Context, req Request, keyID string, key []byte) error {
	opened, err := store.Open(ctx, req.DatabasePath, candidateKey{id: keyID, key: key}, store.Config{Limits: req.Limits})
	if err != nil {
		return fmt.Errorf("validate state key: %w", err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.VerifyStateKey(ctx); err != nil {
		return err
	}
	return nil
}

// candidateKey supplies a planned state key without writing it to a backend.
type candidateKey struct {
	id  string
	key []byte
}

func (c candidateKey) GetStateKey(keyID string) ([]byte, bool, error) {
	if keyID != c.id {
		return nil, false, nil
	}
	return bytes.Clone(c.key), true, nil
}

func (candidateKey) CreateStateKey() (string, []byte, error) {
	return "", nil, errors.New("repair must not create a state key")
}
