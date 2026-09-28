// Package repair migrates credentials stranded in older Linux Secret Service
// collections into the desktop default collection. It never deletes a source
// collection and never generates a replacement state key.
package repair

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

// InteractionBudget is the fixed window for one interactive repair, including
// Secret Service connection setup, property reads, unlock prompts, migration,
// verification, and cleanup. It matches login and cannot be extended.
const InteractionBudget = 2 * time.Minute

// ErrLeaseBusy reports that another credential writer holds the profile lease.
// Repair writes nothing in that case.
var ErrLeaseBusy = errors.New("credential lease is busy")

// Request identifies one existing profile database and its credential namespace.
type Request struct {
	DatabasePath string
	Namespace    string
	Limits       limits.Limits
	// Interactive allows unlocking legacy collections. The repair command sets
	// it; serve must not call this package.
	Interactive bool
}

// Result is the non-secret outcome of a migration.
type Result struct {
	StateKeyID     string
	Copied         int
	AlreadyPresent int
}

// MigrateLegacyKeyring copies the profile state key and OAuth credential
// items into the default collection, then proves a fresh backend handle can
// open the existing database. Candidate bytes are validated before any
// destination write. The profile credential lease is held across the write so
// a concurrent repair or OAuth writer cannot publish a second copy. A failed
// migration leaves source items in place and does not create a replacement
// state key.
func MigrateLegacyKeyring(ctx context.Context, service secretservice.Service, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, InteractionBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	before, err := store.ReadPlaintextIdentity(ctx, req.DatabasePath)
	if err != nil {
		return Result{}, err
	}
	plan, err := secretservice.ReadPlan(ctx, service, migrateRequest(req, before))
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	stateKey, err := validatePlan(ctx, req, before, plan)
	if err != nil {
		return Result{}, err
	}
	writeCtx, release, err := claimCredentialLease(ctx, req, before.StateKeyID, stateKey)
	if err != nil {
		return Result{}, err
	}
	defer release()
	migrated, err := secretservice.WritePlan(writeCtx, service, plan, req.Interactive)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := verifyMigrated(ctx, service, req, before); err != nil {
		return Result{}, err
	}
	after, err := store.ReadPlaintextIdentity(ctx, req.DatabasePath)
	if err != nil {
		return Result{}, err
	}
	if after.StateKeyID != before.StateKeyID {
		return Result{}, errors.New("repair changed the state key identifier")
	}
	return Result{
		StateKeyID:     before.StateKeyID,
		Copied:         migrated.Copied,
		AlreadyPresent: migrated.AlreadyPresent,
	}, nil
}

func (r Request) validate() error {
	if r.DatabasePath == "" {
		return errors.New("profile database path is required")
	}
	if r.Namespace == "" {
		return errors.New("credential namespace is required")
	}
	if err := r.Limits.Validate(); err != nil {
		return fmt.Errorf("repair limits: %w", err)
	}
	return nil
}

func migrateRequest(req Request, identity store.PlaintextIdentity) secretservice.MigrateRequest {
	prefix := req.Namespace + "/"
	required := make([]string, 0, len(identity.LiveSlots))
	for _, slot := range identity.LiveSlots {
		required = append(required, prefix+"secret/"+slot)
	}
	optional := []string{
		prefix + "secret/" + store.RefreshFenceName,
		prefix + "secret/" + store.ClientFenceName,
	}
	for _, slot := range identity.RetiredSlots {
		optional = append(optional, prefix+"secret/"+slot)
	}
	return secretservice.MigrateRequest{
		Prefix:      prefix,
		StateKey:    prefix + "state/" + identity.StateKeyID,
		Required:    required,
		Optional:    optional,
		Interactive: req.Interactive,
	}
}

func verifyMigrated(ctx context.Context, service secretservice.Service, req Request, identity store.PlaintextIdentity) error {
	// A second handle is what a fresh process constructs. It must see the
	// copied items without creating a collection.
	backend, err := secretservice.OpenService(ctx, service, secretservice.Config{Interactive: req.Interactive})
	if err != nil {
		return fmt.Errorf("reopen default collection: %w", err)
	}
	keys := credential.NewWithBackend(req.Namespace, backend)
	st, err := store.Open(ctx, req.DatabasePath, keys, store.Config{Limits: req.Limits})
	if err != nil {
		return fmt.Errorf("verify migrated state key: %w", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.VerifyStateKey(ctx); err != nil {
		return err
	}
	for _, slot := range identity.LiveSlots {
		if err := ctx.Err(); err != nil {
			return err
		}
		secret, found, err := keys.GetSecret(slot)
		if err != nil {
			return fmt.Errorf("verify oauth credential: %w", err)
		}
		if !found || len(secret) == 0 {
			return fmt.Errorf("%w: oauth credential %q is not readable", secretservice.ErrLegacyMissing, slot)
		}
	}
	return nil
}
