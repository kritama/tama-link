package store

import (
	"bytes"
	"testing"
)

func TestCipherBindsBlobToSubmissionAndKind(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	cipher, err := newStateCipher(key)
	if err != nil {
		t.Fatalf("newStateCipher: %v", err)
	}
	sealed, err := cipher.seal([]byte("private"), "sub-1", "result")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := cipher.open(sealed, "sub-2", "result"); err == nil {
		t.Fatal("blob opened under another submission")
	}
	if _, err := cipher.open(sealed, "sub-1", "arguments"); err == nil {
		t.Fatal("result blob opened as arguments")
	}
	got, err := cipher.open(sealed, "sub-1", "result")
	if err != nil || string(got) != "private" {
		t.Fatalf("bound open = %q, %v", got, err)
	}
}
