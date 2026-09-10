package version

import "testing"

func TestString(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := Version, Commit, BuildDate
	t.Cleanup(func() {
		Version, Commit, BuildDate = oldVersion, oldCommit, oldBuildDate
	})

	Version = "v1.2.3"
	Commit = "abc123"
	BuildDate = "2026-09-10T00:00:00Z"

	want := "tama-link v1.2.3 (commit abc123, built 2026-09-10T00:00:00Z)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
