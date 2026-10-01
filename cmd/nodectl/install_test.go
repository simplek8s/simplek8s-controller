package main

// Unit tests for the install pure helpers (PLAN.md §3.15):
// partition naming, mount detection, yaml rendering, salt shape.
// Device writes, network, countdown and password prompt are live
// E2E (§7.8) — they need root + block devices.

import (
	"os"
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
	out := minimalInstallYAML("202609210825", "$6$salt$hash", nil)
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

func TestCollectInstallSSHKeys(t *testing.T) {
	const k1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc user@h"
	const k2 = "ssh-rsa AAAAB3x== other@h"

	// Empty is valid (keys are optional, like the wizard).
	if got, err := collectInstallSSHKeys(nil, ""); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v,%v, want no keys", got, err)
	}
	// Flags only.
	got, err := collectInstallSSHKeys([]string{k1, k2}, "")
	if err != nil || len(got) != 2 || got[0] != k1 || got[1] != k2 {
		t.Fatalf("flags = %v,%v", got, err)
	}
	// Bad flag rejected (same rule as the wizard textarea).
	if _, err := collectInstallSSHKeys([]string{"not-a-key"}, ""); err == nil {
		t.Fatal("bad flag must fail")
	}
	if _, err := collectInstallSSHKeys([]string{"  "}, ""); err == nil {
		t.Fatal("blank flag must fail")
	}
	// File: blank lines ignored, bad line reported with its number.
	dir := t.TempDir()
	file := filepath.Join(dir, "keys")
	if err := os.WriteFile(file, []byte(k1+"\n\n"+k2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = collectInstallSSHKeys(nil, file)
	if err != nil || len(got) != 2 || got[0] != k1 || got[1] != k2 {
		t.Fatalf("file = %v,%v", got, err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte(k1+"\nnot-a-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := collectInstallSSHKeys(nil, bad); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("bad file err = %v, want line 2", err)
	}
	if _, err := collectInstallSSHKeys(nil, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file must fail")
	}
	// File first, then flags.
	got, err = collectInstallSSHKeys([]string{k2}, file)
	if err != nil || len(got) != 3 || got[0] != k1 || got[1] != k2 || got[2] != k2 {
		t.Fatalf("merged = %v,%v", got, err)
	}
}

func TestStringListFlag(t *testing.T) {
	var s stringList
	if err := s.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Fatalf("list = %v", []string(s))
	}
}

func TestParseDiskLabel(t *testing.T) {
	dos := "label: dos\nlabel-id: 0x12345678\ndevice: /dev/sda\nunit: sectors\n\n/dev/sda1 : start=2048, size=1048576, type=ef\n"
	if got := parseDiskLabel(dos); got != "dos" {
		t.Errorf("parseDiskLabel(dos) = %q", got)
	}
	gpt := "label: gpt\nlabel-id: DBF508B4-D465-48E9-AC1A-5B32AD6AE8F3\ndevice: /dev/sda\nunit: sectors\n\n/dev/sda1 : start=2048, size=1048576, type=U\n"
	if got := parseDiskLabel(gpt); got != "gpt" {
		t.Errorf("parseDiskLabel(gpt) = %q", got)
	}
	if got := parseDiskLabel("garbage\n"); got != "" {
		t.Errorf("parseDiskLabel(garbage) = %q, want empty", got)
	}
}

func TestVarPartType(t *testing.T) {
	if got, ok := varPartType("dos"); !ok || got != "83" {
		t.Errorf("varPartType(dos) = (%q,%v)", got, ok)
	}
	// GPT takes the Linux-filesystem GUID: a bare 83 is
	// rejected as Invalid argument (live find on hybrid IMGs).
	if got, ok := varPartType("gpt"); !ok || got != "0FC63DAF-8483-4772-8E79-3D69D8477DE4" {
		t.Errorf("varPartType(gpt) = (%q,%v)", got, ok)
	}
	if _, ok := varPartType("sgi"); ok {
		t.Error("varPartType(sgi) must fail closed")
	}
	if _, ok := varPartType(""); ok {
		t.Error("varPartType(empty) must fail closed")
	}
}
