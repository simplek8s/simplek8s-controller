package updatecore

// Unit tests for the install-image index helpers (PLAN.md §3.15,
// M8 D9): grammar (plain .img + aliases excluded) and newest-TS
// selection.

import (
	"testing"
)

func TestParseImgRelease(t *testing.T) {
	for _, tc := range []struct {
		file     string
		ts, arch string
		ok       bool
	}{
		{"simplek8s.202609210825.x86-64.img.zst", "202609210825", "x86-64", true},
		{"simplek8s.202402070018.rpi4.img.zst", "202402070018", "rpi4", true},
		{"simplek8s.20230214173347.x86-64.img.zst", "", "", false},
		{"simplek8s.20230210212548.x86-64.img.zst", "", "", false},
		{"simplek8s.202402070018.x86-64.img", "", "", false},
		{"simplek8s.latest.x86-64.img", "", "", false},
		{"simplek8s.latest.x86-64.img.zst", "", "", false},
		{"simplek8s.202609210825.x86-64.efi.zst", "", "", false},
		{"SHA256SUMS.gpg", "", "", false},
		{"", "", "", false},
	} {
		ts, arch, ok := ParseImgRelease(tc.file)
		if ts != tc.ts || arch != tc.arch || ok != tc.ok {
			t.Errorf("ParseImgRelease(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.file, ts, arch, ok, tc.ts, tc.arch, tc.ok)
		}
	}
}

func TestFilterImgIndex(t *testing.T) {
	sums := map[string]string{
		"simplek8s.202601010000.x86-64.img.zst": "aa",
		"simplek8s.202602020000.x86-64.img.zst": "bb",
		"simplek8s.202603030000.rpi4.img.zst":   "cc",
		"simplek8s.202602020000.x86-64.img":     "dd",
		"simplek8s.latest.x86-64.img":           "ee",
	}
	ts, file, sum, ok := FilterImgIndex(sums, "x86-64")
	if !ok {
		t.Fatal("want match for x86-64")
	}
	if ts != "202602020000" || file != "simplek8s.202602020000.x86-64.img.zst" || sum != "bb" {
		t.Fatalf("got (%q,%q,%q)", ts, file, sum)
	}
	if _, _, _, ok := FilterImgIndex(sums, "rpi5"); ok {
		t.Fatal("want no match for rpi5")
	}
}

func TestInstallFlavor(t *testing.T) {
	for _, tc := range []struct {
		model, compatible, goarch, want string
		ok                              bool
	}{
		{"Raspberry Pi 5 Model B", "", "arm64", "rpi5", true},
		{"", "brcm,bcm2711", "arm64", "rpi4", true},
		{"", "", "amd64", "x86-64", true},
		{"", "", "arm64", "arm64", true},
		{"", "", "riscv64", "", false},
		// Device-tree wins over build arch.
		{"Raspberry Pi 4 Model B", "", "amd64", "rpi4", true},
	} {
		got, ok := InstallFlavor(tc.model, tc.compatible, tc.goarch)
		if got != tc.want || ok != tc.ok {
			t.Errorf("InstallFlavor(%q,%q,%q) = (%q,%v), want (%q,%v)",
				tc.model, tc.compatible, tc.goarch, got, ok, tc.want, tc.ok)
		}
	}
}
