package secretservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MigrateRequest selects one profile's credential items. Keys are item
// identifiers, never secret values.
type MigrateRequest struct {
	// Prefix is the profile credential namespace including its trailing slash.
	Prefix string
	// StateKey is the full item key of the database's state-encryption key.
	StateKey string
	// Required are item keys that must be readable after migration.
	Required []string
	// Optional are item keys copied when present and unambiguous.
	Optional []string
	// Interactive allows unlocking legacy collections. Serve must not set it.
	Interactive bool
}

// MigrateResult counts items copied or already present. It contains no
// secret material.
type MigrateResult struct {
	Copied         int
	AlreadyPresent int
}

// ItemPlan is one credential item selected for migration. Value is the exact
// source blob. Callers validate it before WritePlan publishes anything.
type ItemPlan struct {
	Key           string
	Value         []byte
	ContentType   string
	InDestination bool
	Required      bool
}

type credentialBlob struct {
	Value       []byte
	ContentType string
}

type valuedItem struct {
	blob    credentialBlob
	sources []string
}

// ReadPlan selects legacy items without writing. A cancelled context returns
// before any collection is unlocked.
func ReadPlan(ctx context.Context, service Service, req MigrateRequest) ([]ItemPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateMigrateRequest(req); err != nil {
		return nil, err
	}
	session, destination, legacy, err := openMigration(ctx, service, req.Interactive)
	if err != nil {
		return nil, err
	}
	return planMigration(ctx, session, destination, legacy, req)
}

// WritePlan publishes an already validated plan. It rechecks the destination
// and writes nothing if the context is cancelled or any destination value
// conflicts. replace stays false: an existing item is never overwritten.
func WritePlan(ctx context.Context, service Service, items []ItemPlan, interactive bool) (MigrateResult, error) {
	if err := ctx.Err(); err != nil {
		return MigrateResult{}, err
	}
	if service == nil {
		return MigrateResult{}, errors.New("secret service is required")
	}
	session, err := service.OpenSession(ctx)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("open secret service session: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return MigrateResult{}, err
	}
	destination, err := service.ReadAlias(ctx, defaultAlias)
	if err != nil {
		return MigrateResult{}, err
	}
	if destination == nil || isRootPath(destination.Path()) {
		return MigrateResult{}, ErrDefaultCollectionMissing
	}
	if err := prepareCollection(ctx, service, destination, interactive); err != nil {
		return MigrateResult{}, err
	}
	pending, already, err := recheckDestination(ctx, session, destination, items)
	if err != nil {
		return MigrateResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return MigrateResult{}, err
	}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return MigrateResult{}, err
		}
		if err := writeExact(ctx, session, destination, item); err != nil {
			return MigrateResult{}, err
		}
	}
	if err := verifyCopied(ctx, session, destination, items); err != nil {
		return MigrateResult{}, err
	}
	return MigrateResult{Copied: len(pending), AlreadyPresent: already}, nil
}

// Migrate plans and writes in one call. Repair validates the plan before
// calling WritePlan so a bad candidate never reaches the destination.
func Migrate(ctx context.Context, service Service, req MigrateRequest) (MigrateResult, error) {
	items, err := ReadPlan(ctx, service, req)
	if err != nil {
		return MigrateResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return MigrateResult{}, err
	}
	return WritePlan(ctx, service, items, req.Interactive)
}

func validateMigrateRequest(req MigrateRequest) error {
	if req.Prefix == "" || !strings.HasSuffix(req.Prefix, "/") {
		return errors.New("credential namespace prefix is required")
	}
	if req.StateKey == "" || !strings.HasPrefix(req.StateKey, req.Prefix) {
		return errors.New("state key item is outside the profile namespace")
	}
	for _, key := range append(append([]string{}, req.Required...), req.Optional...) {
		if !strings.HasPrefix(key, req.Prefix) {
			return fmt.Errorf("credential item %q is outside the profile namespace", key)
		}
	}
	return nil
}

func openMigration(ctx context.Context, service Service, interactive bool) (Session, Collection, []Collection, error) {
	if service == nil {
		return nil, nil, nil, errors.New("secret service is required")
	}
	session, err := service.OpenSession(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open secret service session: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	destination, err := service.ReadAlias(ctx, defaultAlias)
	if err != nil {
		return nil, nil, nil, err
	}
	if destination == nil || isRootPath(destination.Path()) {
		return nil, nil, nil, ErrDefaultCollectionMissing
	}
	if err := prepareCollection(ctx, service, destination, interactive); err != nil {
		return nil, nil, nil, err
	}
	legacy, err := legacyCollections(ctx, service, destination.Path())
	if err != nil {
		return nil, nil, nil, err
	}
	for _, collection := range legacy {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if err := prepareCollection(ctx, service, collection, interactive); err != nil {
			return nil, nil, nil, err
		}
	}
	return session, destination, legacy, nil
}

func legacyCollections(ctx context.Context, service Service, defaultPath string) ([]Collection, error) {
	all, err := service.Collections(ctx)
	if err != nil {
		return nil, fmt.Errorf("list secret service collections: %w", err)
	}
	legacy := make([]Collection, 0)
	for _, collection := range all {
		if collection.Path() == defaultPath {
			continue
		}
		label, err := collection.Label(ctx)
		if err != nil {
			return nil, fmt.Errorf("read collection label %s: %w", collection.Path(), err)
		}
		if label == LegacyCollectionLabel {
			legacy = append(legacy, collection)
		}
	}
	slices.SortFunc(legacy, func(a, b Collection) int {
		return strings.Compare(a.Path(), b.Path())
	})
	return legacy, nil
}

func prepareCollection(ctx context.Context, service Service, collection Collection, interactive bool) error {
	return unlockObject(ctx, service, collection, interactive)
}

func planMigration(ctx context.Context, session Session, destination Collection, legacy []Collection, req MigrateRequest) ([]ItemPlan, error) {
	found := map[string]*valuedItem{}
	for _, collection := range legacy {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := readLegacyItems(ctx, session, collection, req, found); err != nil {
			return nil, err
		}
	}
	keys := migrationKeys(req, found)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := readDestination(ctx, session, destination, key, found); err != nil {
			return nil, err
		}
	}
	return resolvePlan(req, keys, found)
}

func migrationKeys(req MigrateRequest, found map[string]*valuedItem) []string {
	keys := map[string]struct{}{req.StateKey: {}}
	for _, key := range req.Required {
		keys[key] = struct{}{}
	}
	for _, key := range req.Optional {
		if _, ok := found[key]; ok {
			keys[key] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	slices.Sort(ordered)
	return ordered
}

func resolvePlan(req MigrateRequest, keys []string, found map[string]*valuedItem) ([]ItemPlan, error) {
	required := map[string]struct{}{req.StateKey: {}}
	for _, key := range req.Required {
		required[key] = struct{}{}
	}
	planned := make([]ItemPlan, 0, len(keys))
	for _, key := range keys {
		item := found[key]
		_, must := required[key]
		if item == nil || len(item.blob.Value) == 0 {
			if must {
				return nil, fmt.Errorf("%w: item %q", ErrLegacyMissing, key)
			}
			continue
		}
		planned = append(planned, ItemPlan{
			Key:           key,
			Value:         bytes.Clone(item.blob.Value),
			ContentType:   item.blob.ContentType,
			InDestination: slices.Contains(item.sources, destinationSource),
			Required:      must,
		})
	}
	return planned, nil
}

const destinationSource = "default-alias"

func readDestination(ctx context.Context, session Session, destination Collection, key string, found map[string]*valuedItem) error {
	items, err := destination.SearchItems(ctx, map[string]string{itemKeyAttribute: key})
	if err != nil {
		return fmt.Errorf("search default collection: %w", err)
	}
	for _, item := range items {
		blob, err := itemSecret(ctx, session, item)
		if err != nil {
			return err
		}
		if err := addSource(found, key, destinationSource, blob); err != nil {
			return err
		}
	}
	return nil
}
