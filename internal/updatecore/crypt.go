package updatecore

import (
	"crypto/sha512"
	"fmt"
	"strings"
)

// SHA-512-crypt password hashing (PLAN.md §3.15, M8 D5): the install
// yaml carries `password_hash` for root, and init applies it
// verbatim to the shadow entry — so nodectl must produce the
// `$6$salt$hash` string itself (no live-env `openssl` owed,
// single-static-binary discipline, M6 D1).
//
// Straight port of Ulrich Drepper's normative SHA-crypt spec
// (default 5000 rounds, no `rounds=` support): crypto/sha512 does
// the hashing, this file only the crypt structure + crypt-base64
// output. Salt is 1-16 chars of ./0-9A-Za-z (spec charset);
// longer/invalid salts are refused, never truncated.

// cryptRounds is the fixed iteration count (spec default; no custom
// rounds — one code path, one tested shape).
const cryptRounds = 5000

// cryptSaltAlphabet is the spec salt charset.
const cryptSaltAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// cryptB64 is the crypt-base64 alphabet (spec §22e order).
const cryptB64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CryptSHA512 hashes password with salt into a `$6$salt$hash`
// shadow-ready string.
func CryptSHA512(password, salt string) (string, error) {
	if len(salt) == 0 || len(salt) > 16 {
		return "", fmt.Errorf("salt must be 1-16 chars")
	}
	for _, c := range salt {
		if !strings.ContainsRune(cryptSaltAlphabet, c) {
			return "", fmt.Errorf("salt char %q outside ./0-9A-Za-z", c)
		}
	}
	pw := []byte(password)
	s := []byte(salt)

	// Alternate sum B = SHA512(pw + salt + pw) (spec steps 4-8).
	bSum := sha512.Sum512(append(append(append([]byte{}, pw...), s...), pw...))

	// Digest A = pw + salt + key_len bytes of B (cycled from its
	// start) + binary-length mixing (spec steps 1-3, 9-11).
	a := sha512.New()
	a.Write(pw)
	a.Write(s)
	for n := len(pw); n > 0; {
		m := 64
		if n < 64 {
			m = n
		}
		a.Write(bSum[:m])
		n -= m
	}
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(bSum[:])
		} else {
			a.Write(pw)
		}
	}
	aSum := a.Sum(nil)

	// P sequence: DP = SHA512(pw fed key_len times), stretched to
	// key_len (spec steps 13-16).
	dp := sha512.New()
	for i := 0; i < len(pw); i++ {
		dp.Write(pw)
	}
	dpSum := dp.Sum(nil)
	pSeq := make([]byte, 0, len(pw))
	for len(pSeq) < len(pw) {
		pSeq = append(pSeq, dpSum...)
	}
	pSeq = pSeq[:len(pw)]

	// S sequence: DS = SHA512(salt fed 16+A[0] times), stretched
	// to salt_len (spec steps 17-20; A[0] is the first byte of
	// the step-12 digest).
	ds := sha512.New()
	for i := 0; i < 16+int(aSum[0]); i++ {
		ds.Write(s)
	}
	dsSum := ds.Sum(nil)
	sSeq := make([]byte, 0, len(s))
	for len(sSeq) < len(s) {
		sSeq = append(sSeq, dsSum...)
	}
	sSeq = sSeq[:len(s)]

	// Stretching rounds (spec step 21).
	sum := aSum
	for i := 0; i < cryptRounds; i++ {
		c := sha512.New()
		if i&1 != 0 {
			c.Write(pSeq)
		} else {
			c.Write(sum)
		}
		if i%3 != 0 {
			c.Write(sSeq)
		}
		if i%7 != 0 {
			c.Write(pSeq)
		}
		if i&1 != 0 {
			c.Write(sum)
		} else {
			c.Write(pSeq)
		}
		sum = c.Sum(nil)
	}

	return "$6$" + salt + "$" + cryptEncode(sum), nil
}

// cryptTriple holds one (#3,#2,#1) index triple of the spec §22e
// table for SHA-512 (63 digest bytes in 21 triples; byte 63 closes
// with two chars).
var cryptTriples = [21][3]int{
	{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4},
	{47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51},
	{31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
	{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
	{62, 20, 41},
}

// cryptEncode renders the 64-byte digest in crypt-base64 (low bits
// first — the reverse of MIME base64).
func cryptEncode(sum []byte) string {
	var sb strings.Builder
	for _, t := range cryptTriples {
		w := uint32(sum[t[0]])<<16 | uint32(sum[t[1]])<<8 | uint32(sum[t[2]])
		for i := 0; i < 4; i++ {
			sb.WriteByte(cryptB64[w&0x3f])
			w >>= 6
		}
	}
	w := uint32(sum[63])
	for i := 0; i < 2; i++ {
		sb.WriteByte(cryptB64[w&0x3f])
		w >>= 6
	}
	return sb.String()
}
