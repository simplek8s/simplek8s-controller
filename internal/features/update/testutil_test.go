package update

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// testKey is a throwaway GPG identity: the private entity (for signing)
// plus a keyring file (armored by default) that the feature verifies
// against.
type testKey struct {
	entity  *openpgp.Entity
	keyring string // path to the keyring file
}

func newTestKey(t *testing.T, armored bool) *testKey {
	t.Helper()
	e, err := openpgp.NewEntity("simplek8s test", "release signing key", "release@simplek8s.test",
		&packet.Config{RSABits: 2048})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pubring.gpg")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if armored {
		a, err := armor.Encode(w, "PGP PUBLIC KEY BLOCK", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Serialize(a); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := e.Serialize(w); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &testKey{entity: e, keyring: path}
}

// sign returns a detached signature over data (armored or binary).
func (k *testKey) sign(t *testing.T, data []byte, armored bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	if armored {
		err = openpgp.ArmoredDetachSign(&buf, k.entity, bytes.NewReader(data), nil)
	} else {
		err = openpgp.DetachSign(&buf, k.entity, bytes.NewReader(data), nil)
	}
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return buf.Bytes()
}

// sha256line renders one SHA256SUMS line for payload.
func sha256line(payload []byte, name string) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x  %s", sum, name)
}

// releaseRepo serves a signed release index: SHA256SUMS + SHA256SUMS.gpg
// (armored). files maps filename -> payload (any bytes).
func releaseRepo(t *testing.T, key *testKey, armoredSig bool, files map[string][]byte) *httptest.Server {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var index bytes.Buffer
	for _, n := range names {
		index.WriteString(sha256line(files[n], n) + "\n")
	}
	sig := key.sign(t, index.Bytes(), armoredSig)
	mux := http.NewServeMux()
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write(index.Bytes())
	})
	mux.HandleFunc("/SHA256SUMS.gpg", func(w http.ResponseWriter, r *http.Request) {
		w.Write(sig)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// kernelIndex is a realistic index: two x86-64 kernel releases and one
// aarch64 (.efi.zst), plus non-kernel and legacy .kernel files that must
// be ignored.
func kernelIndex() map[string][]byte {
	return map[string][]byte{
		"simplek8s.202601010000.x86-64.efi.zst":    []byte("old-kernel"),
		"simplek8s.202608291203.x86-64.efi.zst":    []byte("new-kernel"),
		"simplek8s.202605050000.aarch64.efi.zst":   []byte("arm-kernel"),
		"simplek8s.202608291203.x86-64.kernel.zst": []byte("legacy"), // legacy .kernel: ignored
		"simplek8s.202608291203.x86-64.img.zst":    []byte("img"),
		"info.json":                                []byte("{}"),
	}
}
