package updatecore

import (
	"os"
	"path/filepath"
	"testing"
)

// recordingLogger captures warnings emitted during verification.
type recordingLogger struct{ warnings []string }

func (l *recordingLogger) Debug(string, ...any) {}
func (l *recordingLogger) Info(string, ...any)  {}
func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.warnings = append(l.warnings, msg)
}

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
	index := []byte("deadbeef  simplek8s.202601010000.x86-64.efi.zst\n")

	for _, armored := range []bool{true, false} {
		k := newTestKey(t, true) // keyring always armored is fine
		el, err := LoadKeyring(k.keyring)
		if err != nil {
			t.Fatal(err)
		}
		sig := k.sign(t, index, armored)
		signer, err := VerifyIndex(nil, el, index, sig)
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
	if _, err := VerifyIndex(nil, el, tampered, sig); err == nil {
		t.Fatal("want error for tampered index")
	}

	// A signature from a different key is rejected (unknown issuer).
	other := newTestKey(t, true)
	foreignSig := other.sign(t, index, true)
	if _, err := VerifyIndex(nil, el, index, foreignSig); err == nil {
		t.Fatal("want error for foreign signer")
	}
}

// TestVerifyIndexAcceptsExpiredKey covers the deployed-tool case: the
// keyring is embedded in the binary, so once its key expires the tool
// can no longer verify the self-update that would replace it. Expiry
// must warn, not reject; the cryptographic checks stay in force.
func TestVerifyIndexAcceptsExpiredKey(t *testing.T) {
	index := []byte("deadbeef  simplek8s.202601010000.x86-64.efi.zst\n")

	for _, armored := range []bool{true, false} {
		k := newExpiredTestKey(t, true)
		el, err := LoadKeyring(k.keyring)
		if err != nil {
			t.Fatal(err)
		}
		sig := k.sign(t, index, armored)
		log := &recordingLogger{}
		signer, err := VerifyIndex(log, el, index, sig)
		if err != nil {
			t.Fatalf("armored=%v: expired key rejected: %v", armored, err)
		}
		if signer == nil {
			t.Fatalf("armored=%v: nil signer", armored)
		}
		if len(log.warnings) != 1 {
			t.Fatalf("armored=%v: want one expiry warning, got %v", armored, log.warnings)
		}

		// Expiry relaxes only the validity period, never the signature.
		tampered := append(append([]byte{}, index...), 'x')
		if _, err := VerifyIndex(nil, el, tampered, sig); err == nil {
			t.Fatalf("armored=%v: want error for tampered index", armored)
		}
		other := newTestKey(t, true)
		if _, err := VerifyIndex(nil, el, index, other.sign(t, index, armored)); err == nil {
			t.Fatalf("armored=%v: want error for foreign signer", armored)
		}
	}
}
