package secretservice

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
)

func readLegacyItems(ctx context.Context, session Session, collection Collection, prefix string, found map[string]*valuedItem) error {
	items, err := collection.Items(ctx)
	if err != nil {
		return fmt.Errorf("list legacy collection %s: %w", collection.Path(), err)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := itemKey(ctx, item)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		blob, err := itemSecret(ctx, session, item)
		if err != nil {
			return err
		}
		if err := addSource(found, key, collection.Path(), blob); err != nil {
			return err
		}
	}
	return nil
}

func itemKey(ctx context.Context, item Item) (string, error) {
	attributes, err := item.Attributes(ctx)
	if err != nil {
		return "", fmt.Errorf("read secret service item attributes: %w", err)
	}
	if key := attributes[itemKeyAttribute]; key != "" {
		return key, nil
	}
	label, err := item.Label(ctx)
	if err != nil {
		return "", fmt.Errorf("read secret service item label: %w", err)
	}
	return label, nil
}

func itemSecret(ctx context.Context, session Session, item Item) (credentialBlob, error) {
	secret, err := item.GetSecret(ctx, session)
	if err != nil {
		return credentialBlob{}, fmt.Errorf("read secret service item %s: %w", item.Path(), err)
	}
	return credentialBlob{Value: bytes.Clone(secret.Value), ContentType: secret.ContentType}, nil
}

func addSource(found map[string]*valuedItem, key, source string, blob credentialBlob) error {
	current := found[key]
	if current == nil {
		found[key] = &valuedItem{blob: blob, sources: []string{source}}
		return nil
	}
	if !bytes.Equal(current.blob.Value, blob.Value) {
		sources := append(append([]string{}, current.sources...), source)
		slices.Sort(sources)
		return fmt.Errorf("%w: item %q differs across %s", ErrLegacyConflict, key, strings.Join(sources, ", "))
	}
	if current.blob.ContentType == "" {
		current.blob.ContentType = blob.ContentType
	}
	if !slices.Contains(current.sources, source) {
		current.sources = append(current.sources, source)
	}
	return nil
}

func recheckDestination(ctx context.Context, session Session, destination Collection, items []ItemPlan) ([]ItemPlan, int, error) {
	pending := make([]ItemPlan, 0)
	already := 0
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		matches, err := destinationSecrets(ctx, session, destination, item.Key)
		if err != nil {
			return nil, 0, err
		}
		switch len(matches) {
		case 0:
			pending = append(pending, item)
		case 1:
			if !bytes.Equal(matches[0], item.Value) {
				return nil, 0, fmt.Errorf("%w: item %q", ErrLegacyConflict, item.Key)
			}
			already++
		default:
			return nil, 0, fmt.Errorf("%w: item %q", ErrDuplicateItem, item.Key)
		}
	}
	return pending, already, nil
}

func destinationSecrets(ctx context.Context, session Session, destination Collection, key string) ([][]byte, error) {
	items, err := destination.SearchItems(ctx, map[string]string{itemKeyAttribute: key})
	if err != nil {
		return nil, fmt.Errorf("search default collection: %w", err)
	}
	values := make([][]byte, 0, len(items))
	for _, item := range items {
		secret, err := item.GetSecret(ctx, session)
		if err != nil {
			return nil, fmt.Errorf("read destination item %q: %w", key, err)
		}
		values = append(values, secret.Value)
	}
	return values, nil
}

func writeExact(ctx context.Context, session Session, destination Collection, item ItemPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	contentType := item.ContentType
	if contentType == "" {
		contentType = secretContentType
	}
	if _, err := destination.CreateItem(ctx, session, item.Key, Secret{
		Value:       bytes.Clone(item.Value),
		ContentType: contentType,
	}, false); err != nil {
		return fmt.Errorf("copy credential item %q: %w", item.Key, err)
	}
	return nil
}

func verifyCopied(ctx context.Context, session Session, destination Collection, planned []ItemPlan) error {
	for _, item := range planned {
		matches, err := destinationSecrets(ctx, session, destination, item.Key)
		if err != nil {
			return fmt.Errorf("verify credential item %q: %w", item.Key, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("%w: item %q was not readable after copy", ErrLegacyMissing, item.Key)
		}
		if len(matches) != 1 {
			return fmt.Errorf("%w: item %q", ErrDuplicateItem, item.Key)
		}
		if !bytes.Equal(matches[0], item.Value) {
			return fmt.Errorf("%w: item %q did not round-trip", ErrLegacyConflict, item.Key)
		}
	}
	return nil
}
