//go:build linux || darwin

package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/profile"
)

func TestJournalRejectsPermissiveStaging(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	dir := filepath.Join(configDir, profile.StagingDirName)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := listJournals(configDir); err == nil {
		t.Fatal("listed a group-readable staging directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("{}\n")
	if err := os.WriteFile(filepath.Join(dir, "tama-app.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ReadPrivateFile(dir, "tama-app.json", 1024); err == nil {
		t.Fatal("read a permissive journal")
	}
	if err := os.Remove(filepath.Join(dir, "tama-app.json")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"id":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "tama-app.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ReadPrivateFile(dir, "tama-app.json", 1024); err == nil {
		t.Fatal("followed a journal symlink")
	}
	swapped := filepath.Join(t.TempDir(), "swapped")
	if err := os.Mkdir(swapped, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(swapped, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := listJournals(configDir); err == nil {
		t.Fatal("followed a swapped staging directory")
	}
}

func TestForeignJournalOwnershipIsRejected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("foreign ownership requires root or a user namespace")
	}
	configDir := privateConfig(t)
	dir := filepath.Join(configDir, profile.StagingDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tama-app.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Skipf("cannot chown journal: %v", err)
	}
	if _, err := profile.ReadPrivateFile(dir, "tama-app.json", 1024); err == nil {
		t.Fatal("read a foreign-owned journal")
	}
}
