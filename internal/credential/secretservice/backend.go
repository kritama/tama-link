package secretservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/99designs/keyring"
)

// backend is one open handle on the Secret Service default collection.
// ctx is stored because keyring.Keyring methods cannot accept one. Repair
// passes a cancellable context; serve uses a background context because the
// keyring API itself cannot be cancelled.
type backend struct {
	ctx         context.Context
	service     Service
	session     Session
	collection  Collection
	interactive bool
}

// Open dials the session bus and opens the default collection. It never
// creates a collection.
func Open(ctx context.Context, cfg Config) (keyring.Keyring, error) {
	service, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	kr, err := OpenService(ctx, service, cfg)
	if err != nil {
		closeService(service)
		return nil, err
	}
	return kr, nil
}

func closeService(service Service) {
	closer, ok := service.(interface{ Close() error })
	if ok {
		_ = closer.Close()
	}
}

// OpenService opens the default alias on an already-connected service. A
// second call returns a new handle over the same collection, which is what a
// separate Tama Link process does.
func OpenService(ctx context.Context, service Service, cfg Config) (keyring.Keyring, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service == nil {
		return nil, errors.New("secret service is required")
	}
	session, err := service.OpenSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("open secret service session: %w", err)
	}
	collection, err := service.ReadAlias(ctx, defaultAlias)
	if err != nil {
		return nil, err
	}
	if collection == nil || isRootPath(collection.Path()) {
		return nil, ErrDefaultCollectionMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &backend{
		ctx:         ctx,
		service:     service,
		session:     session,
		collection:  collection,
		interactive: cfg.Interactive,
	}, nil
}

func (b *backend) Get(key string) (keyring.Item, error) {
	if err := b.ensureUnlocked(); err != nil {
		return keyring.Item{}, err
	}
	items, err := b.matching(key)
	if err != nil {
		return keyring.Item{}, err
	}
	if len(items) == 0 {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	var stored keyring.Item
	var value []byte
	for i, item := range items {
		if err := b.ensureItemUnlocked(item); err != nil {
			return keyring.Item{}, err
		}
		secret, err := item.GetSecret(b.ctx, b.session)
		if err != nil {
			return keyring.Item{}, fmt.Errorf("read secret service item: %w", err)
		}
		if i == 0 {
			value = secret.Value
			if err := json.Unmarshal(secret.Value, &stored); err != nil {
				return keyring.Item{}, errors.New("secret service item is not a credential record")
			}
			continue
		}
		if !bytes.Equal(value, secret.Value) {
			return keyring.Item{}, fmt.Errorf("%w: item %q", ErrDuplicateItem, key)
		}
	}
	return stored, nil
}

func (b *backend) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (b *backend) Set(item keyring.Item) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := b.ensureUnlocked(); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode secret service item: %w", err)
	}
	_, err = b.collection.CreateItem(b.ctx, b.session, item.Key, Secret{
		Value:       data,
		ContentType: secretContentType,
	}, true)
	if err != nil {
		return fmt.Errorf("write secret service item: %w", err)
	}
	return nil
}

func (b *backend) Remove(key string) error {
	if err := b.ensureUnlocked(); err != nil {
		return err
	}
	items, err := b.matching(key)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := b.ensureItemUnlocked(item); err != nil {
			return err
		}
		if err := item.Delete(b.ctx); err != nil {
			return fmt.Errorf("delete secret service item: %w", err)
		}
	}
	return nil
}

func (b *backend) Keys() ([]string, error) {
	if err := b.ensureUnlocked(); err != nil {
		return nil, err
	}
	items, err := b.collection.Items(b.ctx)
	if err != nil {
		return nil, fmt.Errorf("list secret service items: %w", err)
	}
	keys := make([]string, 0, len(items))
	for _, item := range items {
		label, err := item.Label(b.ctx)
		if err != nil {
			return nil, fmt.Errorf("read secret service item label: %w", err)
		}
		keys = append(keys, label)
	}
	return keys, nil
}

func (b *backend) matching(key string) ([]Item, error) {
	items, err := b.collection.SearchItems(b.ctx, map[string]string{itemKeyAttribute: key})
	if err != nil {
		return nil, fmt.Errorf("search secret service item: %w", err)
	}
	return items, nil
}

func (b *backend) ensureUnlocked() error {
	return unlockObject(b.ctx, b.service, b.collection, b.interactive)
}

func (b *backend) ensureItemUnlocked(item Item) error {
	return unlockObject(b.ctx, b.service, item, b.interactive)
}

func unlockObject(ctx context.Context, service Service, object Object, interactive bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	locked, err := lockedOf(ctx, object)
	if err != nil {
		return err
	}
	if !locked {
		return nil
	}
	if !interactive {
		return fmt.Errorf("%w: %s", ErrCollectionLocked, object.Path())
	}
	if err := service.Unlock(ctx, object); err != nil {
		return fmt.Errorf("unlock secret service object %s: %w", object.Path(), err)
	}
	return nil
}

func lockedOf(ctx context.Context, object Object) (bool, error) {
	switch object := object.(type) {
	case Collection:
		locked, err := object.Locked(ctx)
		if err != nil {
			return true, fmt.Errorf("read secret service collection lock: %w", err)
		}
		return locked, nil
	case Item:
		locked, err := object.Locked(ctx)
		if err != nil {
			return true, fmt.Errorf("read secret service item lock: %w", err)
		}
		return locked, nil
	default:
		return true, errors.New("secret service object does not expose a lock")
	}
}

func isRootPath(path string) bool {
	return path == "" || path == "/"
}
