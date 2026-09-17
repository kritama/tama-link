package credential

import (
	"errors"
	"testing"
)

func TestSecretRoundTrip(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())

	if err := kr.SetSecret("oauth-client", []byte(`{"client_id":"c1"}`)); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	data, found, err := kr.GetSecret("oauth-client")
	if err != nil || !found {
		t.Fatalf("GetSecret = found %v err %v", found, err)
	}
	if string(data) != `{"client_id":"c1"}` {
		t.Fatalf("round-trip = %s", data)
	}
	if err := kr.DeleteSecret("oauth-client"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, found, _ := kr.GetSecret("oauth-client"); found {
		t.Fatal("secret survived delete")
	}
	if err := kr.DeleteSecret("oauth-client"); err != nil {
		t.Fatalf("absent DeleteSecret = %v, want nil", err)
	}
}

func TestSecretProfileIsolation(t *testing.T) {
	shared := newFakeKeyring()
	alpha := newWith("alpha", shared)
	beta := newWith("beta", shared)

	if err := alpha.SetSecret("oauth-refresh", []byte("secret-value")); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if _, found, err := beta.GetSecret("oauth-refresh"); err != nil || found {
		t.Fatalf("cross-profile GetSecret = found %v err %v; want false, nil", found, err)
	}
}

func TestSecretLabelPolicy(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())
	for _, label := range []string{"", "a/b", "-lead", "a b", "trailing ", "x" + string(make([]byte, 200))} {
		if _, _, err := kr.GetSecret(label); err == nil {
			t.Errorf("GetSecret(%q) accepted", label)
		}
		if err := kr.SetSecret(label, []byte("data")); err == nil {
			t.Errorf("SetSecret(%q) accepted", label)
		}
		if err := kr.DeleteSecret(label); err == nil {
			t.Errorf("DeleteSecret(%q) accepted", label)
		}
	}
}

func TestSecretBackendUnavailable(t *testing.T) {
	fake := newFakeKeyring()
	kr := newWith("alpha", fake)

	fake.failSet = true
	if err := kr.SetSecret("oauth-client", []byte("data")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("SetSecret backend-down = %v, want ErrUnavailable", err)
	}
	fake.failSet = false
	fake.failGet = true
	if _, _, err := kr.GetSecret("oauth-client"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetSecret backend-down = %v, want ErrUnavailable", err)
	}
}

func TestSecretSetRejectsEmpty(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())
	if err := kr.SetSecret("oauth-client", nil); err == nil {
		t.Fatal("empty secret data accepted")
	}
}
