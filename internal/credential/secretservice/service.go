package secretservice

import (
	"context"
	"errors"
)

const (
	// LegacyCollectionLabel is the collection label older Linux builds created
	// from the credential service name. Migration recognizes it by this label,
	// never by a desktop-specific object path.
	LegacyCollectionLabel = "Tama Link"

	defaultAlias      = "default"
	itemKeyAttribute  = "profile"
	secretContentType = "application/json"
	dbusServiceName   = "org.freedesktop.secrets"
	dbusServicePath   = "/org/freedesktop/secrets"
)

// Errors returned by the Secret Service adapter. None of them carry secret
// bytes, collection passwords, or credential payloads.
var (
	// ErrDefaultCollectionMissing reports that the Secret Service default
	// alias does not resolve to a collection. The adapter does not create one.
	ErrDefaultCollectionMissing = errors.New("secret service default collection alias is missing")
	// ErrCollectionLocked reports that a collection or item is locked and this
	// caller must not prompt.
	ErrCollectionLocked = errors.New("secret service collection is locked")
	// ErrLegacyConflict reports that legacy collections do not agree on a
	// credential item. Sources are left unchanged.
	ErrLegacyConflict = errors.New("legacy secret service credentials conflict")
	// ErrLegacyMissing reports that a required credential item is absent from
	// both the default collection and legacy collections.
	ErrLegacyMissing = errors.New("legacy secret service credential is missing")
	// ErrPromptDismissed reports that the user dismissed a Secret Service
	// prompt. No collection is created.
	ErrPromptDismissed = errors.New("secret service prompt was dismissed")
	// ErrDuplicateItem reports that more than one destination item matches a
	// credential key. Replacement is not used to collapse them.
	ErrDuplicateItem = errors.New("secret service credential item is duplicated")
)

// Config selects how a backend may interact with an existing keyring.
type Config struct {
	// Interactive allows unlocking an existing collection. Unattended serve
	// startup leaves it false so a locked keyring fails without a prompt.
	Interactive bool
}

// Service is the Secret Service surface this adapter needs. It has no
// collection-creation method: a missing default alias is an error. Methods
// honor ctx and must not write after cancellation.
type Service interface {
	OpenSession(ctx context.Context) (Session, error)
	ReadAlias(ctx context.Context, name string) (Collection, error)
	Collections(ctx context.Context) ([]Collection, error)
	Unlock(ctx context.Context, object Object) error
}

// Object is a Secret Service object that can be unlocked.
type Object interface {
	Path() string
}

// Session is an open Secret Service session. Its path is not a secret.
type Session interface {
	Object
}

// Collection is one Secret Service collection.
type Collection interface {
	Object
	Label(ctx context.Context) (string, error)
	Locked(ctx context.Context) (bool, error)
	Items(ctx context.Context) ([]Item, error)
	SearchItems(ctx context.Context, attributes map[string]string) ([]Item, error)
	CreateItem(ctx context.Context, session Session, label string, secret Secret, replace bool) (Item, error)
}

// Item is one Secret Service item.
type Item interface {
	Object
	Label(ctx context.Context) (string, error)
	Attributes(ctx context.Context) (map[string]string, error)
	Locked(ctx context.Context) (bool, error)
	GetSecret(ctx context.Context, session Session) (Secret, error)
	Delete(ctx context.Context) error
}

// Secret is one item payload. Value is the credential blob and must not be
// logged.
type Secret struct {
	Value       []byte
	ContentType string
}
