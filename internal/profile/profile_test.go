package profile

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNameAcceptsPortableNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"a",
		"memovee",
		"tama-app",
		"tama-system",
		"a-b-c",
		"a0",
		strings.Repeat("a", MaxNameLength),
	} {
		parsed, err := ParseName(name)
		if err != nil {
			t.Fatalf("ParseName(%q) unexpected error: %v", name, err)
		}
		if parsed.String() != name {
			t.Fatalf("ParseName(%q) = %q", name, parsed)
		}
	}
}

func TestParseNameRejectsInvalidNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		"A",
		"abcDEF",
		"1abc",
		"-abc",
		"abc_def",
		"abc.def",
		"abc def",
		"abc/def",
		"abc\ndef",
		"a-b-c-",
		strings.Repeat("a", MaxNameLength+1),
	} {
		if _, err := ParseName(name); err == nil {
			t.Fatalf("ParseName(%q) accepted an invalid name", name)
		}
	}
}

func TestProfilePath(t *testing.T) {
	t.Parallel()

	got := Path("/cfg", Name("tama-app"))
	want := filepath.Join("/cfg", ProfilesDirName, "tama-app"+ProfileFileSuffix)
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestConfigDirOverride(t *testing.T) {
	t.Parallel()

	got, err := ConfigDir("/explicit")
	if err != nil {
		t.Fatalf("ConfigDir(override) unexpected error: %v", err)
	}
	if got != "/explicit" {
		t.Fatalf("ConfigDir(override) = %q", got)
	}
}

func TestConfigDirDefaultIsPlatformScoped(t *testing.T) {
	t.Parallel()

	got, err := ConfigDir("")
	if err != nil {
		t.Fatalf("ConfigDir() unexpected error: %v", err)
	}
	if filepath.Base(got) != ConfigDirName {
		t.Fatalf("ConfigDir() = %q, want suffix %q", got, ConfigDirName)
	}
}

func TestStateDirOverride(t *testing.T) {
	t.Parallel()

	got, err := StateDir("/explicit")
	if err != nil {
		t.Fatalf("StateDir(override) unexpected error: %v", err)
	}
	if got != "/explicit" {
		t.Fatalf("StateDir(override) = %q", got)
	}
}

func TestStateDirDefaultIsPlatformScoped(t *testing.T) {
	t.Parallel()

	got, err := StateDir("")
	if err != nil {
		t.Fatalf("StateDir() unexpected error: %v", err)
	}
	if filepath.Base(got) != ConfigDirName {
		t.Fatalf("StateDir() = %q, want suffix %q", got, ConfigDirName)
	}
}
