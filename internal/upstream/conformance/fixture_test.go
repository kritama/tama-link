package conformance

import "testing"

func TestPinsMatchImmutableRevisions(t *testing.T) {
	if err := verifyPins(); err != nil {
		t.Fatal(err)
	}
}

func TestPackageFixtures(t *testing.T) {
	set := mustLoad(t)
	for _, fx := range set.Wire() {
		t.Run(fx.Name, func(t *testing.T) {
			t.Parallel()
			runFixture(t, fx)
		})
	}
}

func TestLegacyFixturesAreNotEmitted(t *testing.T) {
	set := mustLoad(t)
	var names []string
	for _, fx := range set.Wire() {
		if fx.legacy() {
			names = append(names, fx.Name)
			if !forbiddenMethod(fx.Method()) {
				t.Errorf("%s method %s is not forbidden", fx.Name, fx.Method())
			}
		}
	}
	if len(names) != 2 {
		t.Fatalf("legacy fixtures = %v, want tasks/result and tasks/list", names)
	}
}

func mustLoad(t *testing.T) Set {
	t.Helper()
	set, err := loadSet()
	if err != nil {
		t.Fatal(err)
	}
	return set
}
