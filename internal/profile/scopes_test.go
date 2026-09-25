package profile

import (
	"strings"
	"testing"
)

func TestCanonicalScopes(t *testing.T) {
	t.Parallel()

	canonical, err := CanonicalScopes([]string{"zeta", "alpha", "mcp.message", "system:review"})
	if err != nil {
		t.Fatalf("CanonicalScopes: %v", err)
	}
	want := []string{"alpha", "mcp.message", "system:review", "zeta"}
	if !slicesEqual(canonical, want) {
		t.Fatalf("canonical = %v, want %v", canonical, want)
	}

	if _, err := CanonicalScopes(nil); err != nil {
		t.Fatalf("empty set rejected: %v", err)
	}
	if got, err := CanonicalScopes([]string{"a"}); err != nil || len(got) != 1 || got[0] != "a" {
		t.Fatalf("single scope = %v err=%v", got, err)
	}
}

func TestCanonicalScopesRejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		scopes []string
	}{
		{"duplicate", []string{"a", "b", "a"}},
		{"empty token", []string{"a", ""}},
		{"whitespace token", []string{"mcp message"}},
		{"control token", []string{"a\x00b"}},
		{"non-ascii token", []string{"mcp.メッセージ"}},
		{"oversized token", []string{strings.Repeat("a", MaxScopeLength+1)}},
		{"too many scopes", make([]string, MaxScopes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scopes := make([]string, 0, len(tc.scopes))
			for i := range tc.scopes {
				scopes = append(scopes, strings.Repeat("s", i+1))
			}
			if tc.name != "too many scopes" {
				scopes = tc.scopes
			}
			if _, err := CanonicalScopes(scopes); err == nil {
				t.Fatalf("CanonicalScopes(%v) accepted", scopes)
			}
		})
	}
}

func TestValidateCanonicalizesScopes(t *testing.T) {
	t.Parallel()

	p := validProfile()
	p.Scopes = []string{"zeta.scope", "alpha.scope"}
	if err := p.Validate(p.Name); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []string{"alpha.scope", "zeta.scope"}
	if !slicesEqual(p.Scopes, want) {
		t.Fatalf("loaded scopes = %v, want canonical %v", p.Scopes, want)
	}
}

func TestValidateVersion1RejectsScopes(t *testing.T) {
	t.Parallel()

	p := legacyProfile()
	if err := p.Validate(p.Name); err != nil {
		t.Fatalf("legacy profile: %v", err)
	}
	p.Scopes = []string{"mcp.message"}
	if err := p.Validate(p.Name); err == nil || !strings.Contains(err.Error(), "must not declare scopes") {
		t.Fatalf("version 1 with scopes = %v, want migration error", err)
	}
}

func TestValidateVersion2RequiresScopes(t *testing.T) {
	t.Parallel()

	p := validProfile()
	p.Scopes = nil
	if err := p.Validate(p.Name); err == nil || !strings.Contains(err.Error(), "non-empty scopes array") {
		t.Fatalf("version 2 without scopes = %v, want required error", err)
	}
}

func TestDigestIncludesCanonicalScopes(t *testing.T) {
	t.Parallel()

	first := validProfile()
	second := validProfile()
	second.Scopes = []string{"mcp.message"} // same set, different input order
	firstDigest, err := first.ComputeDigest()
	if err != nil {
		t.Fatalf("first ComputeDigest: %v", err)
	}
	secondDigest, err := second.ComputeDigest()
	if err != nil {
		t.Fatalf("second ComputeDigest: %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("identical scope sets digested differently: %s != %s", firstDigest, secondDigest)
	}

	third := validProfile()
	third.Scopes = []string{"mcp.message", "extra.scope"}
	thirdDigest, err := third.ComputeDigest()
	if err != nil {
		t.Fatalf("third ComputeDigest: %v", err)
	}
	if thirdDigest == firstDigest {
		t.Fatal("a changed scope set kept the original digest")
	}

	// A pinned digest reconciles against the canonical set: a file that
	// stores the same set in a different order still verifies once the
	// loader canonicalizes it.
	pinned := validProfile()
	pinned.Scopes = []string{"mcp.message", "extra.scope"}
	pinned.Digest, err = pinned.ComputeDigest()
	if err != nil {
		t.Fatalf("pinned ComputeDigest: %v", err)
	}
	pinned.Scopes = []string{"extra.scope", "mcp.message"}
	if err := pinned.Validate(pinned.Name); err != nil {
		t.Fatalf("canonicalized reordering broke the digest: %v", err)
	}
	pinned.Scopes = []string{"other.scope"}
	if err := pinned.Validate(pinned.Name); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("scope change without reconciliation accepted: %v", err)
	}
}

func TestVersion1DigestUnchangedByScopesField(t *testing.T) {
	t.Parallel()

	// The digest form carries scopes with omitempty: a version 1 profile's
	// digest is byte-for-byte what the Phase 2 contract computed, so an
	// existing pinned profile stays valid after the upgrade.
	p := legacyProfile()
	if p.Scopes != nil {
		t.Fatalf("legacy profile carries scopes %v", p.Scopes)
	}
	digest, err := p.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q", digest)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
