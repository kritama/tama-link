package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func privateConfig(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPublishCreatesReloadableProfile(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	p := validProfile()
	digest, err := p.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	p.Digest = digest
	if err := Publish(configDir, p); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	loaded, err := Load(p.Name, configDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Endpoint != p.Endpoint || loaded.Digest != digest {
		t.Fatalf("loaded = %+v", loaded)
	}
	info, err := os.Stat(Path(configDir, p.Name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %04o", info.Mode().Perm())
	}
}

func TestPublishDoesNotReplaceExistingProfile(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	first := validProfile()
	digest, err := first.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	first.Digest = digest
	if err := Publish(configDir, first); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(Path(configDir, first.Name))
	if err != nil {
		t.Fatal(err)
	}
	second := validProfile()
	second.Instructions = "changed"
	secondDigest, err := second.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	second.Digest = secondDigest
	if err := Publish(configDir, second); err == nil {
		t.Fatal("second Publish succeeded")
	}
	got, err := os.ReadFile(Path(configDir, first.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("winner changed:\n%s", got)
	}
}

func TestPublishRejectsSymlinkTarget(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	profiles := filepath.Join(configDir, ProfilesDirName)
	if err := os.Mkdir(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(profiles, "tama-app.json")); err != nil {
		t.Fatal(err)
	}
	p := validProfile()
	digest, err := p.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	p.Digest = digest
	if err := Publish(configDir, p); err == nil {
		t.Fatal("publish followed or replaced a symlink")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nope" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestListReportsInvalidProfiles(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	p := validProfile()
	digest, err := p.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	p.Digest = digest
	if err := Publish(configDir, p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, ProfilesDirName, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := List(configDir)
	if err != nil {
		t.Fatal(err)
	}
	var valid, invalid int
	for _, entry := range entries {
		if entry.Valid() {
			valid++
		} else {
			invalid++
		}
	}
	if valid != 1 || invalid != 1 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseOriginRejectsEndpointPath(t *testing.T) {
	t.Parallel()
	_, err := ParseOrigin("https://tama.example/mcp/app")
	if err == nil {
		t.Fatal("accepted an endpoint path")
	}
	origin, err := ParseOrigin("https://tama.example/")
	if err != nil {
		t.Fatal(err)
	}
	if origin != "https://tama.example" {
		t.Fatalf("origin = %s", origin)
	}
	endpoint, err := EndpointFor(origin, KindSystem)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://tama.example/mcp/system" {
		t.Fatalf("endpoint = %s", endpoint)
	}
}
