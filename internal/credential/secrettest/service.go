// Package secrettest provides an in-memory Secret Service fixture. The
// user-visible collection label and the D-Bus object path are independent, so
// tests can prove the adapter follows the default alias instead of a derived
// path.
package secrettest

import (
	"context"
	"fmt"
	"sync"

	"github.com/kritama/tama-link/internal/credential/secretservice"
)

// Service is an in-memory org.freedesktop.secrets service. It has no method
// that creates a collection on behalf of the adapter. CreateItem with
// replace=false appends another item with the same attributes, matching the
// Secret Service contract rather than rejecting the duplicate.
type Service struct {
	mu          sync.Mutex
	nextItem    int
	nextSess    int
	unlocks     int
	creates     int
	deletes     int
	defaultPath string
	order       []string
	collections map[string]*Collection
	hook        func(context.Context, string) error
}

// New returns an empty Secret Service with no default alias.
func New() *Service {
	return &Service{collections: map[string]*Collection{}}
}

// SetGate installs a hook invoked before session, unlock, and item creation.
// A hook that waits on ctx proves cancellation does not continue into writes.
func (s *Service) SetGate(hook func(context.Context, string) error) {
	s.mu.Lock()
	s.hook = hook
	s.mu.Unlock()
}

func (s *Service) enter(ctx context.Context, op string) error {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, op)
}

// SetDefault installs the collection behind the default alias. The path is
// not derived from the label.
func (s *Service) SetDefault(path, label string) *Collection {
	collection := s.AddCollection(path, label)
	s.mu.Lock()
	s.defaultPath = path
	s.mu.Unlock()
	return collection
}

// AddCollection adds one collection. Adding the same path twice panics; tests
// should treat paths as stable object identities.
func (s *Service) AddCollection(path, label string) *Collection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.collections[path]; ok {
		panic("duplicate secret service collection path " + path)
	}
	collection := &Collection{
		service: s,
		path:    path,
		label:   label,
		items:   map[string][]*item{},
	}
	s.collections[path] = collection
	s.order = append(s.order, path)
	return collection
}

// Collection returns a collection by object path.
func (s *Service) Collection(path string) *Collection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collections[path]
}

// CollectionCount is the number of collections. Repeated adapter opens must
// not change it.
func (s *Service) CollectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.collections)
}

// UnlockCalls counts Unlock invocations, including those the adapter must not
// make while unattended.
func (s *Service) UnlockCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unlocks
}

// CreateItemCalls counts item writes.
func (s *Service) CreateItemCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

// DeleteCalls counts item deletions. Migration must leave this at zero.
func (s *Service) DeleteCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletes
}

// OpenSession implements secretservice.Service.
func (s *Service) OpenSession(ctx context.Context) (secretservice.Session, error) {
	if err := s.enter(ctx, "session"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSess++
	return session{path: fmt.Sprintf("/session/%d", s.nextSess)}, nil
}

// ReadAlias implements secretservice.Service. Only the default alias resolves.
func (s *Service) ReadAlias(ctx context.Context, name string) (secretservice.Collection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "default" || s.defaultPath == "" {
		return nil, secretservice.ErrDefaultCollectionMissing
	}
	collection := s.collections[s.defaultPath]
	if collection == nil {
		return nil, secretservice.ErrDefaultCollectionMissing
	}
	return collection, nil
}

// Collections implements secretservice.Service.
func (s *Service) Collections(ctx context.Context) ([]secretservice.Collection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	collections := make([]secretservice.Collection, 0, len(s.order))
	for _, path := range s.order {
		collections = append(collections, s.collections[path])
	}
	return collections, nil
}

// Unlock implements secretservice.Service.
func (s *Service) Unlock(ctx context.Context, object secretservice.Object) error {
	if err := s.enter(ctx, "unlock"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unlocks++
	switch object := object.(type) {
	case *Collection:
		if object.unlockErr != nil {
			return object.unlockErr
		}
		object.locked = false
		for _, group := range object.items {
			for _, stored := range group {
				stored.locked = false
			}
		}
		return nil
	case *item:
		object.locked = false
		return nil
	default:
		return fmt.Errorf("unsupported secret service object %T", object)
	}
}

type session struct{ path string }

func (s session) Path() string { return s.path }
