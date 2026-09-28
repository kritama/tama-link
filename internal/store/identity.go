package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// PlaintextIdentity is the non-secret credential binding stored in a profile
// database. It contains no key material or OAuth payloads.
type PlaintextIdentity struct {
	StateKeyID   string
	LiveSlots    []string
	RetiredSlots []string
}

// ReadPlaintextIdentity reads the state-key identifier and credential-fence
// slots from an existing profile database. It does not create a database, a
// state key, or a schema.
func ReadPlaintextIdentity(ctx context.Context, path string) (PlaintextIdentity, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PlaintextIdentity{}, fmt.Errorf("%w: profile database does not exist", ErrStateUnavailable)
		}
		return PlaintextIdentity{}, fmt.Errorf("inspect profile database: %w", err)
	}
	secured, err := secureStatePath(path)
	if err != nil {
		return PlaintextIdentity{}, err
	}
	defer func() { _ = secured.Close() }()
	db, err := openSQLite(ctx, secured)
	if err != nil {
		return PlaintextIdentity{}, err
	}
	defer func() { _ = db.Close() }()
	return readPlaintextIdentity(ctx, db)
}

func readPlaintextIdentity(ctx context.Context, db *sql.DB) (PlaintextIdentity, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return PlaintextIdentity{}, fmt.Errorf("acquire state connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := validateRequiredTables(ctx, conn); err != nil {
		return PlaintextIdentity{}, err
	}
	identity, err := readIdentityMeta(ctx, db)
	if err != nil {
		return PlaintextIdentity{}, err
	}
	live, retired, err := readIdentitySlots(ctx, db)
	if err != nil {
		return PlaintextIdentity{}, err
	}
	identity.LiveSlots = live
	identity.RetiredSlots = retired
	return identity, nil
}

func readIdentityMeta(ctx context.Context, db *sql.DB) (PlaintextIdentity, error) {
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaSchemaVersion).Scan(&schema); err != nil {
		return PlaintextIdentity{}, fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaSchemaVersion, err)
	}
	version, err := parseSchemaVersion(schema)
	if err != nil {
		return PlaintextIdentity{}, fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	if version != schemaVersion {
		return PlaintextIdentity{}, fmt.Errorf("%w: database has %d", ErrUnsupportedSchema, version)
	}
	var keyID string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaStateKeyID).Scan(&keyID); err != nil {
		return PlaintextIdentity{}, fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaStateKeyID, err)
	}
	if keyID == "" || len(keyID) > maxKeyID {
		return PlaintextIdentity{}, fmt.Errorf("%w: metadata %q is invalid", ErrStateUnavailable, metaStateKeyID)
	}
	var format string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		return PlaintextIdentity{}, fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaEncryptionFormat, err)
	}
	parsed, err := strconv.Atoi(format)
	if err != nil || parsed != encryptionFormat {
		return PlaintextIdentity{}, fmt.Errorf("%w: unsupported encryption format %q", ErrStateUnavailable, format)
	}
	return PlaintextIdentity{StateKeyID: keyID, LiveSlots: []string{}, RetiredSlots: []string{}}, nil
}

func readIdentitySlots(ctx context.Context, db *sql.DB) ([]string, []string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, generation, slot FROM credential_fence ORDER BY name")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: read credential fences: %w", ErrStateUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	live := []string{}
	retired := []string{}
	for rows.Next() {
		var name, slot string
		var generation int64
		if err := rows.Scan(&name, &generation, &slot); err != nil {
			return nil, nil, fmt.Errorf("%w: read credential fences: %w", ErrStateUnavailable, err)
		}
		if liveSlot, retiredSlot := classifyFence(name, generation, slot); liveSlot != "" {
			live = append(live, liveSlot)
		} else if retiredSlot != "" {
			retired = append(retired, retiredSlot)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: read credential fences: %w", ErrStateUnavailable, err)
	}
	slices.Sort(live)
	slices.Sort(retired)
	return live, retired, nil
}

func classifyFence(name string, generation int64, slot string) (string, string) {
	if slot == "" {
		return "", ""
	}
	switch name {
	case RefreshFenceName, ClientFenceName:
		if generation > 0 {
			return slot, ""
		}
	}
	if strings.Contains(name, "-retired:") {
		return "", slot
	}
	return "", ""
}
