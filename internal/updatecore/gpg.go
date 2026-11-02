// GPG verification of the release index (PLAN-M2 3.6): mandatory, no
// skip. Keyring resolution: optional custom keyring file (Secret
// simplek8s-controller-keyring, for custom repos/keys) over the
// embedded keyring baked into the image.
package updatecore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
)

// DefaultKeyringPaths are the fixed, non-configurable keyring locations
// (PLAN-M2 3.6/3.14).
const (
	EmbeddedKeyringPath = "/etc/simplek8s/pubring.gpg"
	CustomKeyringPath   = "/etc/simplek8s/custom/pubring.gpg"
)

// ResolveKeyring picks the keyring: the custom file when it exists,
// else the embedded one. Both missing is an error (verification cannot
// be performed).
func ResolveKeyring(custom, embedded string) (string, error) {
	if _, err := os.Stat(custom); err == nil {
		return custom, nil
	}
	if _, err := os.Stat(embedded); err == nil {
		return embedded, nil
	}
	return "", fmt.Errorf("no keyring: %s or %s", custom, embedded)
}

// LoadKeyring reads an armored or binary keyring file.
func LoadKeyring(path string) (openpgp.EntityList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read keyring: %w", err)
	}
	defer f.Close()
	el, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil {
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return nil, fmt.Errorf("read keyring %s: %w", path, err)
		}
		if el, err = openpgp.ReadKeyRing(f); err != nil {
			return nil, fmt.Errorf("parse keyring %s: %w", path, err)
		}
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("keyring %s is empty", path)
	}
	return el, nil
}

// VerifyIndex checks the detached GPG signature of the index. The
// signature may be armored or binary. A bad signature (or unknown
// issuer) rejects the whole index: nothing from it is ever trusted.
//
// Expiry (of the signing key or of the signature) is deliberately
// non-fatal: the signature is still cryptographically valid, and the
// keyring is embedded in the binary that verifies it, so an expired
// key cannot be replaced by the very self-update it gates. Treating
// expiry as fatal would brick already-deployed tools permanently.
// Revocation and every other verification failure remain fatal.
func VerifyIndex(log Logger, keyring openpgp.EntityList, index, signature []byte) (*openpgp.Entity, error) {
	signer, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(index), detachedSignatureReader(signature), nil)
	if err != nil {
		if errors.Is(err, openpgperrors.ErrKeyExpired) || errors.Is(err, openpgperrors.ErrSignatureExpired) {
			if log != nil {
				log.Warn("release index signature accepted despite expiry; renew the signing key", "err", err)
			}
			return signer, nil
		}
		return nil, fmt.Errorf("index signature verification failed: %w", err)
	}
	return signer, nil
}

// detachedSignatureReader unwraps an armored signature body when
// present; binary signatures pass through untouched.
func detachedSignatureReader(b []byte) io.Reader {
	if ab, err := armor.Decode(bytes.NewReader(b)); err == nil && ab != nil {
		return ab.Body
	}
	return bytes.NewReader(b)
}
