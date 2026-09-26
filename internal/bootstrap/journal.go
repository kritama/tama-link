package bootstrap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kritama/tama-link/internal/profile"
)

const (
	journalVersion  = 1
	maxJournalBytes = 16 << 10

	stageReserved   = "reserved"
	stageAuthorized = "authorized"

	stateDatabase    = "default"
	stateCredentials = "default"
)

// journal is the non-secret record of an unfinished bootstrap. It is not a
// profile and is never loaded by profile.Load or serve.
type journal struct {
	Version     int      `json:"version"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Origin      string   `json:"origin"`
	Endpoint    string   `json:"endpoint"`
	Issuer      string   `json:"issuer"`
	Template    string   `json:"template"`
	Scopes      []string `json:"scopes"`
	Database    string   `json:"database"`
	Credentials string   `json:"credentials"`
	Stage       string   `json:"stage"`
}

func (j journal) validate() error {
	if j.Version != journalVersion {
		return fmt.Errorf("unsupported bootstrap journal version %d", j.Version)
	}
	if j.ID == "" || len(j.ID) > 64 {
		return fmt.Errorf("bootstrap journal has an invalid id")
	}
	name, err := profile.ParseName(j.Name)
	if err != nil {
		return err
	}
	if _, err := profile.ParseOrigin(j.Origin); err != nil {
		return err
	}
	if _, err := profile.ParseSecureURL("endpoint", j.Endpoint, true); err != nil {
		return err
	}
	if _, err := profile.ParseSecureURL("issuer", j.Issuer, true); err != nil {
		return err
	}
	if j.Template != "app" && j.Template != "system-read" {
		return fmt.Errorf("bootstrap journal has an unknown template")
	}
	if _, err := profile.CanonicalScopes(j.Scopes); err != nil || len(j.Scopes) == 0 {
		return fmt.Errorf("bootstrap journal has invalid scopes")
	}
	if j.Database != stateDatabase || j.Credentials != stateCredentials {
		return fmt.Errorf("bootstrap journal has unexpected state references")
	}
	if j.Stage != stageReserved && j.Stage != stageAuthorized {
		return fmt.Errorf("bootstrap journal has an unknown stage")
	}
	if name.String() != j.Name {
		return fmt.Errorf("bootstrap journal name does not match")
	}
	return nil
}

func stagingPath(configDir string) (string, error) {
	root, err := profile.ConfigDir(configDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, profile.StagingDirName), nil
}

func journalFile(name profile.Name) string { return name.String() + ".json" }

func (j journal) create(configDir string) error {
	if err := j.validate(); err != nil {
		return err
	}
	dir, err := prepareStaging(configDir)
	if err != nil {
		return err
	}
	data, err := encodeJournal(j)
	if err != nil {
		return err
	}
	if err := profile.WritePrivateFile(dir, journalFile(profile.Name(j.Name)), data, false); err != nil {
		if errors.Is(err, profile.ErrExists) {
			return failErr(fmt.Errorf("%w: %s", ErrBusy, j.Name))
		}
		return failErr(fmt.Errorf("create bootstrap journal: %w", err))
	}
	return nil
}

func (j journal) save(configDir string) error {
	if err := j.validate(); err != nil {
		return err
	}
	dir, err := prepareStaging(configDir)
	if err != nil {
		return err
	}
	data, err := encodeJournal(j)
	if err != nil {
		return err
	}
	if err := profile.WritePrivateFile(dir, journalFile(profile.Name(j.Name)), data, true); err != nil {
		return failErr(fmt.Errorf("update bootstrap journal: %w", err))
	}
	return nil
}

func removeJournal(configDir string, name profile.Name) error {
	dir, err := stagingPath(configDir)
	if err != nil {
		return err
	}
	if err := profile.RemovePrivateFile(dir, journalFile(name)); err != nil {
		return failErr(fmt.Errorf("remove bootstrap journal: %w", err))
	}
	return nil
}

func loadJournal(configDir string, name profile.Name) (journal, error) {
	dir, err := stagingPath(configDir)
	if err != nil {
		return journal{}, err
	}
	data, err := profile.ReadPrivateFile(dir, journalFile(name), maxJournalBytes)
	if err != nil {
		return journal{}, err
	}
	return decodeJournal(data)
}

func listJournals(configDir string) ([]profile.Name, error) {
	dir, err := stagingPath(configDir)
	if err != nil {
		return nil, err
	}
	entries, err := profile.ListPrivateNames(dir)
	if err != nil {
		return nil, err
	}
	var names []profile.Name
	for _, entry := range entries {
		if !strings.HasSuffix(entry, ".json") {
			continue
		}
		name, err := profile.ParseName(strings.TrimSuffix(entry, ".json"))
		if err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names, nil
}

func prepareStaging(configDir string) (string, error) {
	root, err := profile.ConfigDir(configDir)
	if err != nil {
		return "", err
	}
	if err := profile.EnsurePrivateDir(root); err != nil {
		return "", failErr(fmt.Errorf("prepare configuration directory: %w", err))
	}
	dir := filepath.Join(root, profile.StagingDirName)
	if err := profile.EnsurePrivateDir(dir); err != nil {
		return "", failErr(fmt.Errorf("prepare bootstrap staging directory: %w", err))
	}
	return dir, nil
}

func newJournalID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("bootstrap id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func encodeJournal(j journal) ([]byte, error) {
	data, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("encode bootstrap journal: %w", err)
	}
	return append(data, '\n'), nil
}

func decodeJournal(data []byte) (journal, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var j journal
	if err := dec.Decode(&j); err != nil {
		return journal{}, fmt.Errorf("decode bootstrap journal: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return journal{}, fmt.Errorf("bootstrap journal contains trailing data")
	}
	if err := j.validate(); err != nil {
		return journal{}, err
	}
	return j, nil
}
