package main

// Unit tests for the install pure helpers (PLAN.md §3.15):
// partition naming, mount detection, yaml rendering, salt shape.
// Device writes, network, countdown and password prompt are live
// E2E (§7.8) — they need root + block devices.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPartDevName(t *testing.T) {
	for _, tc := range []struct {
		disk string
		num  int
		want string
	}{
		{"/dev/sda", 1, "/dev/sda1"},
		{"/dev/vda", 2, "/dev/vda2"},
		{"/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"/dev/nvme0n1", 2, "/dev/nvme0n1p2"},
		{"/dev/mmcblk0", 1, "/dev/mmcblk0p1"},
		{"/dev/loop0", 2, "/dev/loop0p2"},
	} {
		if got := partDevName(tc.disk, tc.num); got != tc.want {
			t.Errorf("partDevName(%q,%d) = %q, want %q", tc.disk, tc.num, got, tc.want)
		}
	}
}

func TestInstallTargetMounted(t *testing.T) {
	mounts := `/dev/vda1 / ext4 rw 0 0
/dev/vda2 /var ext4 rw 0 0
/dev/sr0 /media iso9660 ro 0 0
/dev/nvme0n1p1 /boot vfat rw 0 0
`
	for _, target := range []string{"/dev/vda", "/dev/sr0", "/dev/nvme0n1"} {
		if !installTargetMounted(mounts, target, filepath.Base(target)) {
			t.Errorf("installTargetMounted(%q) = false, want true", target)
		}
	}
	// Prefix collisions must not match: /dev/vda must not fire on
	// /dev/vdaa, and unrelated devices stay silent.
	other := "/dev/vdaa1 /data ext4 rw 0 0\n/dev/sdb1 /backup ext4 rw 0 0\n"
	if installTargetMounted(other, "/dev/vda", "vda") {
		t.Error("installTargetMounted(/dev/vda) fired on /dev/vdaa1")
	}
	if installTargetMounted(other, "/dev/vdb", "vdb") {
		t.Error("installTargetMounted(/dev/vdb) fired without match")
	}
	if installTargetMounted("", "/dev/vda", "vda") {
		t.Error("installTargetMounted on empty mounts fired")
	}
}

func TestMountinfoHitsTarget(t *testing.T) {
	// Real shape from the live SimpleK8s node that slipped
	// through: everything mounted via by-label aliases, majors
	// 252 (vda) vs 252 (vdb) distinguished only by minor.
	mountinfo := `22 1 252:2 / /var rw,relatime - ext4 /dev/disk/by-label/var rw
23 1 252:1 / /etc rw,relatime - ext4 /dev/disk/by-label/var rw
24 1 0:5 / /proc rw,relatime - proc proc rw
25 1 252:18 / /mnt rw,relatime - ext4 /dev/disk/by-label/var rw
`
	vda := map[[2]int]bool{{252, 0}: true, {252, 1}: true, {252, 2}: true}
	vdb := map[[2]int]bool{{252, 16}: true, {252, 17}: true, {252, 18}: true}
	if !mountinfoHitsTarget(mountinfo, vda) {
		t.Error("vda set must hit (252:1, 252:2 mounted)")
	}
	if !mountinfoHitsTarget(mountinfo, vdb) {
		t.Error("vdb set must hit (252:18 mounted)")
	}
	other := map[[2]int]bool{{8, 1}: true}
	if mountinfoHitsTarget(mountinfo, other) {
		t.Error("foreign set must stay silent despite by-label sources")
	}
	if mountinfoHitsTarget("", vda) {
		t.Error("empty mountinfo must stay silent")
	}
	if mountinfoHitsTarget("garbage line\n22 1 nope / x - t s\n", vda) {
		t.Error("malformed lines must stay silent")
	}
}

func TestSplitDevNo(t *testing.T) {
	for _, tc := range []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"8:1", 8, 1, true},
		{"252:18", 252, 18, true},
		{"0:5", 0, 5, true},
		{"8", 0, 0, false},
		{"x:1", 0, 0, false},
		{"", 0, 0, false},
	} {
		maj, min, ok := splitDevNo(tc.in)
		if maj != tc.maj || min != tc.min || ok != tc.ok {
			t.Errorf("splitDevNo(%q) = (%d,%d,%v)", tc.in, maj, min, ok)
		}
	}
}

func TestMountSourceMatchesTarget(t *testing.T) {
	// Sources arrive symlink-resolved here: by-uuid/by-label land
	// as /dev/<disk>[p]<n> (the live SimpleK8s mounts everything
	// via by-label aliases, which the literal pass never sees).
	for _, tc := range []struct {
		src, target string
		want        bool
	}{
		{"/dev/vda", "/dev/vda", true},
		{"/dev/vda2", "/dev/vda", true},
		{"/dev/nvme0n1p1", "/dev/nvme0n1", true},
		{"/dev/vdaa1", "/dev/vda", false},
		{"/dev/vdb1", "/dev/vda", false},
		{"overlay", "/dev/vda", false},
		{"tmpfs", "/dev/vda", false},
		{"/dev/vda", "/dev/vda1", false},
		{"", "/dev/vda", false},
	} {
		if got := mountSourceMatchesTarget(tc.src, tc.target); got != tc.want {
			t.Errorf("mountSourceMatchesTarget(%q,%q) = %v, want %v", tc.src, tc.target, got, tc.want)
		}
	}
}

func TestMinimalInstallYAML(t *testing.T) {
	out := minimalInstallYAML("202609210825", "$6$salt$hash")
	for _, want := range []string{
		"# Written by nodectl install (202609210825).",
		"users:\n  - name: root\n    password_hash: $6$salt$hash\n",
		"storage:\n  mounts:\n    - what: /dev/disk/by-label/var\n      where: /var\n",
		"simplek8s.yaml.example",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("minimal yaml missing %q:\n%s", want, out)
		}
	}
}

func TestCryptSalt(t *testing.T) {
	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		s, err := cryptSalt()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != installCryptSaltLen {
			t.Fatalf("salt len = %d", len(s))
		}
		for _, c := range s {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("salt char %q outside alphabet", c)
			}
		}
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Fatal("salts are not random")
	}
}
