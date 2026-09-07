package update

import "testing"

func TestParseKernelRelease(t *testing.T) {
	cases := []struct {
		file     string
		ts, arch string
		ok       bool
	}{
		{"simplek8s.202608291203.x86-64.kernel.zst", "202608291203", "x86-64", true},
		{"simplek8s.202608291203.aarch64.kernel.xz", "202608291203", "aarch64", true},
		{"simplek8s.202608291203.x86-64.efi.zst", "", "", false},
		{"simplek8s.202608291203.x86-64.img.zst", "", "", false},
		{"info.json", "", "", false},
		{"simplek8s.202608291203.x86-64.kernel.gz", "", "", false},
		{"simplek8s.202608291203.kernel.zst", "", "", false},
		{"simplek8s.x86-64.kernel.zst", "", "", false},
	}
	for _, tc := range cases {
		ts, arch, ok := ParseKernelRelease(tc.file)
		if ok != tc.ok || ts != tc.ts || arch != tc.arch {
			t.Errorf("ParseKernelRelease(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.file, ts, arch, ok, tc.ts, tc.arch, tc.ok)
		}
	}
}

func TestMapArch(t *testing.T) {
	if a, ok := MapArch("amd64"); !ok || a != "x86-64" {
		t.Errorf("MapArch(amd64) = (%q,%v)", a, ok)
	}
	if a, ok := MapArch("arm64"); !ok || a != "aarch64" {
		t.Errorf("MapArch(arm64) = (%q,%v)", a, ok)
	}
	if _, ok := MapArch("s390x"); ok {
		t.Error("MapArch(s390x) should not map")
	}
	if _, ok := MapArch(""); ok {
		t.Error("MapArch(\"\") should not map")
	}
}

func TestRunningVersion(t *testing.T) {
	cases := []struct {
		kernel string
		want   string
	}{
		{"6.18.48-simplek8s-202608291203 (amd64)", "202608291203"},
		{"6.18.48 (amd64)", ""},
		{"", ""},
		{"5.10.0-simplek8s-202501010101 (aarch64)", "202501010101"},
	}
	for _, tc := range cases {
		if got := RunningVersion(tc.kernel); got != tc.want {
			t.Errorf("RunningVersion(%q) = %q, want %q", tc.kernel, got, tc.want)
		}
	}
}

func TestNewerTS(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"202608291203", "202601010000", true},
		{"202601010000", "202608291203", false},
		{"202601010000", "202601010000", false},
		// Unequal widths compare numerically, not lexicographically
		// (lexicographically "9" > "10").
		{"9", "10", false},
		{"10", "9", true},
		{"banana", "10", false},
		{"10", "banana", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := NewerTS(tc.a, tc.b); got != tc.want {
			t.Errorf("NewerTS(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
