package updatecore

import "testing"

func TestParseKernelRelease(t *testing.T) {
	cases := []struct {
		file     string
		ts, arch string
		ok       bool
	}{
		{"simplek8s.202608291203.x86-64.efi.zst", "202608291203", "x86-64", true},
		{"simplek8s.202608291203.aarch64.efi.xz", "202608291203", "aarch64", true},
		{"simplek8s.202608291203.x86-64.efi", "", "", false},
		{"simplek8s.202608291203.x86-64.img.zst", "", "", false},
		{"info.json", "", "", false},
		{"simplek8s.202608291203.x86-64.efi.gz", "", "", false},
		{"simplek8s.202608291203.x86-64.kernel.zst", "", "", false},
		{"simplek8s.202608291203.efi.zst", "", "", false},
		{"simplek8s.x86-64.efi.zst", "", "", false},
	}
	for _, tc := range cases {
		ts, arch, ok := ParseKernelRelease(tc.file)
		if ok != tc.ok || ts != tc.ts || arch != tc.arch {
			t.Errorf("ParseKernelRelease(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.file, ts, arch, ok, tc.ts, tc.arch, tc.ok)
		}
	}
}

func TestParseStoredKernel(t *testing.T) {
	cases := []struct {
		file       string
		ts, flavor string
		ok         bool
	}{
		{"simplek8s.202608291203.x86-64.efi", "202608291203", "x86-64", true},
		{"simplek8s.202608241828.rpi4.efi", "202608241828", "rpi4", true},
		{"simplek8s.202608032153.rpi5.efi", "202608032153", "rpi5", true},
		{"simplek8s.202605050000.arm64.efi", "202605050000", "arm64", true},
		{"simplek8s.latest.rpi5.efi", "", "", false},
		{"simplek8s.yaml", "", "", false},
		{"vmlinuz", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		ts, flavor, ok := ParseStoredKernel(tc.file)
		if ts != tc.ts || flavor != tc.flavor || ok != tc.ok {
			t.Errorf("ParseStoredKernel(%q) = (%q,%q,%v)", tc.file, ts, flavor, ok)
		}
	}
}

func TestResolveFlavor(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
		ok    bool
	}{
		{"x86-64", []string{"simplek8s.202608291203.x86-64.efi"}, "x86-64", true},
		{"rpi4", []string{"simplek8s.202608241828.rpi4.efi", "simplek8s.yaml"}, "rpi4", true},
		{"first wins sorted", []string{"simplek8s.202608291203.x86-64.efi", "simplek8s.202608241828.rpi4.efi"}, "x86-64", true},
		{"unknown flavor skipped", []string{"simplek8s.202608291203.future.efi", "simplek8s.202608241828.rpi4.efi"}, "rpi4", true},
		{"empty", nil, "", false},
		{"foreign only", []string{"simplek8s.yaml", "vmlinuz", "simplek8s.latest.rpi5.efi"}, "", false},
		{"unknown only", []string{"simplek8s.202608291203.future.efi"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveFlavor(tc.files)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ResolveFlavor = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
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
