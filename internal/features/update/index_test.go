package update

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestParseIndex(t *testing.T) {
	a := sha256.Sum256([]byte("a"))
	b := sha256.Sum256([]byte("b"))
	content := fmt.Sprintf(`# comment line
%[1]x  simplek8s.202601010000.x86-64.efi.zst
%[2]x *simplek8s.202602020000.x86-64.efi.zst
not a sum line
%[1]X  UPPERCASE-HEX-NORMALIZED

`, a, b)
	got, err := ParseIndex([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"simplek8s.202601010000.x86-64.efi.zst": fmt.Sprintf("%x", a),
		"simplek8s.202602020000.x86-64.efi.zst": fmt.Sprintf("%x", b),
		"UPPERCASE-HEX-NORMALIZED":              fmt.Sprintf("%x", a),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries: %v", len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("entry %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseIndexEmpty(t *testing.T) {
	got, err := ParseIndex([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestParseIndexCRLF(t *testing.T) {
	s := sha256.Sum256([]byte("x"))
	got, err := ParseIndex([]byte(fmt.Sprintf("%x  file.bin\r\n", s)))
	if err != nil {
		t.Fatal(err)
	}
	if got["file.bin"] != fmt.Sprintf("%x", s) {
		t.Fatalf("CRLF line not parsed: %v", got)
	}
}
