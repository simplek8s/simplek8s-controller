package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveKeyring(t *testing.T) {
	tmp := t.TempDir()
	custom := filepath.Join(tmp, "custom.gpg")
	embedded := filepath.Join(tmp, "embedded.gpg")
	for _, p := range []string{custom, embedded} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Both present: custom wins.
	if got, err := ResolveKeyring(custom, embedded); err != nil || got != custom {
		t.Fatalf("both: (%q,%v)", got, err)
	}
	// Only embedded.
	os.Remove(custom)
	if got, err := ResolveKeyring(custom, embedded); err != nil || got != embedded {
		t.Fatalf("embedded only: (%q,%v)", got, err)
	}
	// Neither: error (verification is mandatory, not skippable).
	os.Remove(embedded)
	if _, err := ResolveKeyring(custom, embedded); err == nil {
		t.Fatal("want error when no keyring exists")
	}
}

func TestLoadKeyringArmoredAndBinary(t *testing.T) {
	for _, armored := range []bool{true, false} {
		label := "binary"
		if armored {
			label = "armored"
		}
		k := newTestKey(t, armored)
		el, err := LoadKeyring(k.keyring)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(el) != 1 {
			t.Fatalf("%s: %d entities", label, len(el))
		}
	}
}

func TestLoadKeyringErrors(t *testing.T) {
	if _, err := LoadKeyring("/nonexistent/pubring.gpg"); err == nil {
		t.Fatal("want error for missing file")
	}
	empty := filepath.Join(t.TempDir(), "empty.gpg")
	if err := os.WriteFile(empty, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(empty); err == nil {
		t.Fatal("want error for empty keyring")
	}
}

func TestVerifyIndex(t *testing.T) {
	index := []byte("deadbeef  simplek8s.202601010000.x86-64.kernel.zst\n")

	for _, armored := range []bool{true, false} {
		k := newTestKey(t, true) // keyring always armored is fine
		el, err := LoadKeyring(k.keyring)
		if err != nil {
			t.Fatal(err)
		}
		sig := k.sign(t, index, armored)
		signer, err := VerifyIndex(el, index, sig)
		if err != nil {
			t.Fatalf("armored=%v: verify: %v", armored, err)
		}
		if signer == nil {
			t.Fatalf("armored=%v: nil signer", armored)
		}
	}

	// Tampered index is rejected.
	k := newTestKey(t, true)
	el, _ := LoadKeyring(k.keyring)
	sig := k.sign(t, index, true)
	tampered := append(append([]byte{}, index...), 'x')
	if _, err := VerifyIndex(el, tampered, sig); err == nil {
		t.Fatal("want error for tampered index")
	}

	// A signature from a different key is rejected (unknown issuer).
	other := newTestKey(t, true)
	foreignSig := other.sign(t, index, true)
	if _, err := VerifyIndex(el, index, foreignSig); err == nil {
		t.Fatal("want error for foreign signer")
	}
}
