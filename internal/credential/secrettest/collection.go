package secrettest

import (
	"context"
	"fmt"

	"github.com/kritama/tama-link/internal/credential/secretservice"
)

// Collection is one in-memory collection. Its object path is independent of
// its label.
type Collection struct {
	service   *Service
	path      string
	label     string
	locked    bool
	unlockErr error
	items     map[string][]*item
	itemOrder []*item
}

// SetLocked marks the collection locked without prompting.
func (c *Collection) SetLocked(locked bool) {
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	c.locked = locked
}

// SetUnlockError makes Unlock fail and leave the collection locked.
func (c *Collection) SetUnlockError(err error) {
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	c.unlockErr = err
}

// Put stores one exact secret blob under key, replacing any previous matches.
func (c *Collection) Put(key string, value []byte, contentType string) {
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	for _, previous := range c.items[key] {
		c.itemOrder = removeItem(c.itemOrder, previous)
	}
	delete(c.items, key)
	c.putLocked(key, value, contentType, true)
}

// Get returns the first secret blob stored under key.
func (c *Collection) Get(key string) ([]byte, bool) {
	values := c.Values(key)
	if len(values) == 0 {
		return nil, false
	}
	return values[0], true
}

// Values returns every secret blob stored under key, in creation order.
func (c *Collection) Values(key string) [][]byte {
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	group := c.items[key]
	values := make([][]byte, 0, len(group))
	for _, stored := range group {
		values = append(values, append([]byte{}, stored.secret.Value...))
	}
	return values
}

// Len returns the number of items in the collection.
func (c *Collection) Len() int {
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	return len(c.itemOrder)
}

// Path implements secretservice.Collection.
func (c *Collection) Path() string { return c.path }

// Label implements secretservice.Collection.
func (c *Collection) Label(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	return c.label, nil
}

// Locked implements secretservice.Collection.
func (c *Collection) Locked(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	return c.locked, nil
}

// Items implements secretservice.Collection.
func (c *Collection) Items(ctx context.Context) ([]secretservice.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	if c.locked {
		return nil, fmt.Errorf("%w: %s", secretservice.ErrCollectionLocked, c.path)
	}
	items := make([]secretservice.Item, 0, len(c.itemOrder))
	for _, stored := range c.itemOrder {
		items = append(items, stored)
	}
	return items, nil
}

// SearchItems implements secretservice.Collection. It returns every match.
func (c *Collection) SearchItems(ctx context.Context, attributes map[string]string) ([]secretservice.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	if c.locked {
		return nil, fmt.Errorf("%w: %s", secretservice.ErrCollectionLocked, c.path)
	}
	group := c.items[attributes["profile"]]
	items := make([]secretservice.Item, 0, len(group))
	for _, stored := range group {
		items = append(items, stored)
	}
	return items, nil
}

// CreateItem implements secretservice.Collection. replace=false appends a
// duplicate instead of rejecting it, which is what Secret Service does.
func (c *Collection) CreateItem(ctx context.Context, session secretservice.Session, label string, secret secretservice.Secret, replace bool) (secretservice.Item, error) {
	if err := c.service.enter(ctx, "create"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.service.mu.Lock()
	defer c.service.mu.Unlock()
	if c.locked {
		return nil, fmt.Errorf("%w: %s", secretservice.ErrCollectionLocked, c.path)
	}
	if session == nil || session.Path() == "" {
		return nil, fmt.Errorf("create item: session is required")
	}
	c.service.creates++
	return c.putLocked(label, secret.Value, secret.ContentType, replace), nil
}

func (c *Collection) putLocked(key string, value []byte, contentType string, replace bool) *item {
	if contentType == "" {
		contentType = "application/json"
	}
	stored := &item{
		collection: c,
		path:       fmt.Sprintf("%s/%d", c.path, c.service.nextItem+1),
		label:      key,
		attributes: map[string]string{"profile": key},
		secret: secretservice.Secret{
			Value:       append([]byte{}, value...),
			ContentType: contentType,
		},
	}
	c.service.nextItem++
	if replace {
		for _, previous := range c.items[key] {
			c.itemOrder = removeItem(c.itemOrder, previous)
		}
		c.items[key] = []*item{stored}
	} else {
		c.items[key] = append(c.items[key], stored)
	}
	c.itemOrder = append(c.itemOrder, stored)
	return stored
}

func removeItem(order []*item, target *item) []*item {
	filtered := order[:0]
	for _, stored := range order {
		if stored != target {
			filtered = append(filtered, stored)
		}
	}
	return filtered
}

type item struct {
	collection *Collection
	path       string
	label      string
	attributes map[string]string
	secret     secretservice.Secret
	locked     bool
}

func (item *item) Path() string { return item.path }

func (item *item) Label(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return item.label, nil
}

func (item *item) Attributes(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cloned := make(map[string]string, len(item.attributes))
	for key, value := range item.attributes {
		cloned[key] = value
	}
	return cloned, nil
}

func (item *item) Locked(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return item.locked || item.collection.locked, nil
}

func (item *item) GetSecret(ctx context.Context, _ secretservice.Session) (secretservice.Secret, error) {
	if err := ctx.Err(); err != nil {
		return secretservice.Secret{}, err
	}
	item.collection.service.mu.Lock()
	defer item.collection.service.mu.Unlock()
	if item.locked || item.collection.locked {
		return secretservice.Secret{}, fmt.Errorf("%w: %s", secretservice.ErrCollectionLocked, item.path)
	}
	return secretservice.Secret{
		Value:       append([]byte{}, item.secret.Value...),
		ContentType: item.secret.ContentType,
	}, nil
}

func (item *item) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	item.collection.service.mu.Lock()
	defer item.collection.service.mu.Unlock()
	item.collection.service.deletes++
	group := item.collection.items[item.label]
	filtered := group[:0]
	for _, stored := range group {
		if stored != item {
			filtered = append(filtered, stored)
		}
	}
	if len(filtered) == 0 {
		delete(item.collection.items, item.label)
	} else {
		item.collection.items[item.label] = filtered
	}
	item.collection.itemOrder = removeItem(item.collection.itemOrder, item)
	return nil
}
