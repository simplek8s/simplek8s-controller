package updatecore

// Unit tests for the nodectl release helpers (PLAN.md §3.14/§4.6):
// filename grammar (aliases excluded), the GOARCH mapping, and the
// newest-TS selection shared with the kernel rule.

import (
	"testing"
)

func TestParseNodectlRelease(t *testing.T) {
	for _, tc := range []struct {
		file     string
		ts, arch string
		ok       bool
	}{
		{"nodectl.202609171200.x86-64", "202609171200", "x86-64", true},
		{"nodectl.202609171200.arm64", "202609171200", "arm64", true},
		{"nodectl.20260917120012.x86-64", "", "", false},
		{"nodectl.latest.x86-64", "", "", false},
		{"nodectl.202609171200.x86-64.upx", "", "", false},
		{"simplek8s.202609171200.x86-64.efi.zst", "", "", false},
		{"SHA256SUMS.gpg", "", "", false},
		{"", "", "", false},
	} {
		ts, arch, ok := ParseNodectlRelease(tc.file)
		if ts != tc.ts || arch != tc.arch || ok != tc.ok {
			t.Errorf("ParseNodectlRelease(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.file, ts, arch, ok, tc.ts, tc.arch, tc.ok)
		}
	}
}

func TestNodectlArch(t *testing.T) {
	for _, tc := range []struct {
		goarch, want string
		ok           bool
	}{
		{"amd64", "x86-64", true},
		{"arm64", "arm64", true},
		{"riscv64", "", false},
		{"386", "", false},
		{"", "", false},
	} {
		got, ok := NodectlArch(tc.goarch)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NodectlArch(%q) = (%q,%v), want (%q,%v)",
				tc.goarch, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFilterNodectlIndex(t *testing.T) {
	sums := map[string]string{
		"nodectl.202601010000.x86-64": "aa",
		"nodectl.202602020000.x86-64": "bb",
		"nodectl.202603030000.arm64":  "cc",
		"nodectl.latest.x86-64":       "dd",
		"SHA256SUMS.gpg":              "ee",
	}
	ts, file, sum, ok := FilterNodectlIndex(sums, "x86-64")
	if !ok {
		t.Fatal("want match for x86-64")
	}
	if ts != "202602020000" || file != "nodectl.202602020000.x86-64" || sum != "bb" {
		t.Fatalf("got (%q,%q,%q)", ts, file, sum)
	}
	if _, _, _, ok := FilterNodectlIndex(sums, "rpi4"); ok {
		t.Fatal("want no match for rpi4")
	}
	if _, _, _, ok := FilterNodectlIndex(nil, "x86-64"); ok {
		t.Fatal("want no match on empty index")
	}
}
