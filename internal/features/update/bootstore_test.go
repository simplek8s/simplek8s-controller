package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBlkidPrefersEFI(t *testing.T) {
	out := `DEVNAME="/dev/nvme0n1p1"
LABEL="boot"
UUID="aaaa"
DEVNAME="/dev/nvme0n1p2"
LABEL="EFI"
UUID="bbbb"`
	if dev, ok := parseBlkid(out); !ok || dev != "/dev/nvme0n1p2" {
		t.Fatalf("parseBlkid = %q, %v; want EFI device", dev, ok)
	}
}

func TestParseBlkidFallsBackToBoot(t *testing.T) {
	out := `DEVNAME="/dev/sda1"
UUID="aaaa"
DEVNAME="/dev/sda2"
LABEL="boot"`
	if dev, ok := parseBlkid(out); !ok || dev != "/dev/sda2" {
		t.Fatalf("parseBlkid = %q, %v; want boot device", dev, ok)
	}
}

func TestParseBlkidNone(t *testing.T) {
	out := `DEVNAME="/dev/sda1"
LABEL="root"
UUID="aaaa"`
	if dev, ok := parseBlkid(out); ok {
		t.Fatalf("parseBlkid = %q, %v; want no match", dev, ok)
	}
}

func TestParseBlkidIgnoresLabelWithoutDevice(t *testing.T) {
	out := `LABEL="EFI"
DEVNAME="/dev/sdb1"
LABEL="boot"`
	if dev, ok := parseBlkid(out); !ok || dev != "/dev/sdb1" {
		t.Fatalf("parseBlkid = %q, %v; want the labeled device", dev, ok)
	}
}

// mountTracker is a fake Mount/Unmount seam that materializes fake
// partition contents per device, so candidate verification runs
// without loop devices. valid reports whether dev should look like
// our partition (kernel dir + bootloader config); fail forces a mount
// error for dev.
type mountTracker struct {
	mounts   []string
	unmounts []string
	valid    func(dev string) bool
	fail     func(dev string) bool
}

func (m *mountTracker) mount(device, target, fstype string) error {
	m.mounts = append(m.mounts, device)
	if m.fail != nil && m.fail(device) {
		return errFakeMount
	}
	if m.valid != nil && !m.valid(device) {
		return nil // mounted, but empty (decoy)
	}
	if err := os.MkdirAll(filepath.Join(target, "simplek8s"), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(target, "grub"), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(target, "grub", "grub.cfg"), []byte("set default=0\n"), 0644)
}

func (m *mountTracker) unmount(target string) error {
	m.unmounts = append(m.unmounts, target)
	return nil
}

// balanced reports every mount was released (no mounts left behind).
func (m *mountTracker) balanced() bool { return len(m.mounts) == len(m.unmounts) }

var errFakeMount = errors.New("fake mount failure")

// newTestStore builds a store with injected seams over a temp /dev root.
// Every device verifies by default (override tr.valid/tr.fail per test).
func newTestStore(t *testing.T, blkid func(args ...string) (string, error)) (*PhysicalStore, *mountTracker) {
	t.Helper()
	tr := &mountTracker{valid: func(string) bool { return true }}
	s := NewPhysicalStore(PhysicalStoreConfig{
		DevRoot:   t.TempDir(),
		MountRoot: t.TempDir(),
		Blkid:     blkid,
		Log:       discardLogger{},
		Mount:     tr.mount,
		Unmount:   tr.unmount,
	})
	return s, tr
}

// mkLink creates a udev-style symlink (by-label/by-partlabel) whose
// target need not exist (canonicalKey reads the link only).
func mkLink(t *testing.T, devRoot, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(devRoot, filepath.Dir(link)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(devRoot, link)); err != nil {
		t.Fatal(err)
	}
}

func emptyBlkid(args ...string) (string, error) { return "", nil }

func TestFindBootDeviceByLabelSymlink(t *testing.T) {
	s, tr := newTestStore(t, emptyBlkid)
	mkLink(t, s.devRoot, filepath.Join("disk", "by-label", "EFI"), "/dev/nvme0n1p2")
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if filepath.Base(dev) != "EFI" {
		t.Fatalf("findBootDevice = %q, want the by-label EFI symlink", dev)
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceBlkidFallback(t *testing.T) {
	called := false
	s, tr := newTestStore(t, func(args ...string) (string, error) {
		called = true
		return `DEVNAME="/dev/sda2"
LABEL="boot"`, nil
	})
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if !called {
		t.Fatal("blkid was not consulted (no by-label symlink present)")
	}
	if dev != "/dev/sda2" {
		t.Fatalf("findBootDevice = %q, want /dev/sda2", dev)
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceNone(t *testing.T) {
	s, _ := newTestStore(t, func(args ...string) (string, error) {
		return "", nil
	})
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error, want an error (no labeled device)")
	}
}

func TestFindBootDeviceBlkidError(t *testing.T) {
	s, _ := newTestStore(t, func(args ...string) (string, error) {
		return "", errors.New("blkid not found")
	})
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error, want an error (blkid failed)")
	}
}

func TestParseBlkidCandidatesOrder(t *testing.T) {
	out := `DEVNAME="/dev/sda1"
LABEL="boot"
DEVNAME="/dev/sda2"
LABEL="EFI"
DEVNAME="/dev/nvme0n1p1"
PARTLABEL="boot"`
	got := parseBlkidCandidates(out)
	want := []string{"/dev/nvme0n1p1", "/dev/sda2", "/dev/sda1"}
	if len(got) != len(want) {
		t.Fatalf("parseBlkidCandidates = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseBlkidCandidates = %q, want %q", got, want)
		}
	}
}

func TestParseBlkidCandidatesPartlabelBeatsLabel(t *testing.T) {
	// A device carrying PARTLABEL=boot wins its bucket even when its
	// LABEL would sort it elsewhere; stray labels never match.
	out := `DEVNAME="/dev/sda1"
LABEL="EFI"
PARTLABEL="boot"
DEVNAME="/dev/sda2"
LABEL="root"
DEVNAME="/dev/sda3"
LABEL="boot"`
	got := parseBlkidCandidates(out)
	want := []string{"/dev/sda1", "/dev/sda3"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseBlkidCandidates = %q, want %q", got, want)
	}
}

func TestFindBootDevicePrefersPartlabel(t *testing.T) {
	s, tr := newTestStore(t, emptyBlkid)
	mkLink(t, s.devRoot, filepath.Join("disk", "by-partlabel", "boot"), "/dev/vda1")
	mkLink(t, s.devRoot, filepath.Join("disk", "by-label", "EFI"), "/dev/vda2")
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if filepath.Base(dev) != "boot" || !strings.Contains(dev, "by-partlabel") {
		t.Fatalf("findBootDevice = %q, want the by-partlabel boot symlink", dev)
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceDedupsSameDevice(t *testing.T) {
	s, tr := newTestStore(t, emptyBlkid)
	mkLink(t, s.devRoot, filepath.Join("disk", "by-partlabel", "boot"), "/dev/vda1")
	mkLink(t, s.devRoot, filepath.Join("disk", "by-label", "EFI"), "/dev/vda1")
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if !strings.Contains(dev, "by-partlabel") {
		t.Fatalf("findBootDevice = %q, want the first (by-partlabel) form", dev)
	}
	if len(tr.mounts) != 1 {
		t.Fatalf("same device mounted %d times, want 1 (dedup)", len(tr.mounts))
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceSkipsDecoy(t *testing.T) {
	// LABEL=EFI decoy (empty when mounted) sorts before the valid
	// LABEL=boot device: verification must skip it, not first-match it.
	s, tr := newTestStore(t, func(args ...string) (string, error) {
		return `DEVNAME="/dev/decoy1"
LABEL="EFI"
DEVNAME="/dev/sda2"
LABEL="boot"`, nil
	})
	tr.valid = func(dev string) bool { return dev != "/dev/decoy1" }
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if dev != "/dev/sda2" {
		t.Fatalf("findBootDevice = %q, want /dev/sda2 (decoy skipped)", dev)
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceNoneVerifying(t *testing.T) {
	blkidCalls := 0
	s, tr := newTestStore(t, func(args ...string) (string, error) {
		blkidCalls++
		return `DEVNAME="/dev/sda1"
LABEL="boot"`, nil
	})
	tr.valid = func(string) bool { return false }
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error, want fail-closed (no verifying candidate)")
	}
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error on retry, want fail-closed")
	}
	if blkidCalls != 2 {
		t.Fatalf("failure must not cache: blkid calls = %d, want 2", blkidCalls)
	}
	if !tr.balanced() {
		t.Fatalf("mounts left behind: %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

func TestFindBootDeviceCached(t *testing.T) {
	blkidCalls := 0
	s, tr := newTestStore(t, func(args ...string) (string, error) {
		blkidCalls++
		return `DEVNAME="/dev/sda1"
LABEL="boot"`, nil
	})
	first, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	second, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice (cached): %v", err)
	}
	if first != second {
		t.Fatalf("cached device changed: %q vs %q", first, second)
	}
	if blkidCalls != 1 {
		t.Fatalf("blkid calls = %d, want 1 (per-pod cache)", blkidCalls)
	}
	if len(tr.mounts) != 1 {
		t.Fatalf("verification mounts = %d, want 1 (cached, no re-verify)", len(tr.mounts))
	}
}

func TestMountedBootReresolvesAfterMountFailure(t *testing.T) {
	blkidDev := "/dev/sda1"
	s, tr := newTestStore(t, func(args ...string) (string, error) {
		return "DEVNAME=\"" + blkidDev + "\"\nLABEL=\"boot\"", nil
	})
	if _, err := s.findBootDevice(context.Background()); err != nil {
		t.Fatalf("pre-cache findBootDevice: %v", err)
	}
	// sda1 vanishes (mounts fail); sdb1 appears with valid contents.
	blkidDev = "/dev/sdb1"
	tr.fail = func(dev string) bool { return dev == "/dev/sda1" }
	mnt, cleanup, err := s.mountedBoot(context.Background())
	if err != nil {
		t.Fatalf("mountedBoot: %v", err)
	}
	if _, err := os.Stat(mnt); err != nil {
		t.Fatalf("mountedBoot returned unusable mountpoint: %v", err)
	}
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice after re-resolve: %v", err)
	}
	if dev != "/dev/sdb1" {
		t.Fatalf("cached device = %q, want /dev/sdb1", dev)
	}
	cleanup()
	// Exactly one gap: the failed sda1 mount attempt (nothing was
	// mounted, so nothing to unmount); every successful mount released.
	if len(tr.mounts)-len(tr.unmounts) != 1 {
		t.Fatalf("want one unreleased mount (the failed attempt): %d mounts, %d unmounts", len(tr.mounts), len(tr.unmounts))
	}
}

// TestMountDeviceCreatesMissingMountRoot covers the pod /run case: the
// mount root (e.g. /run/simplek8s-controller) does not pre-exist, so
// mountDevice must create it before os.MkdirTemp (which does not make
// parents). Without the fix every scan/stage failed at the mountpoint step.
func TestMountDeviceCreatesMissingMountRoot(t *testing.T) {
	base := t.TempDir()
	mountRoot := filepath.Join(base, "does-not-exist", "simplek8s-controller")
	mounted := false
	s := NewPhysicalStore(PhysicalStoreConfig{
		DevRoot:   t.TempDir(),
		MountRoot: mountRoot,
		Log:       discardLogger{},
		Mount: func(device, target, fstype string) error {
			mounted = true
			return nil
		},
		Unmount: func(target string) error { return nil },
		Blkid:   func(args ...string) (string, error) { return "", nil },
	})
	path, cleanup, err := s.mountDevice("/dev/vda1")
	if err != nil {
		t.Fatalf("mountDevice: %v", err)
	}
	if !mounted {
		t.Fatal("mount seam was not called")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mountpoint missing: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mountpoint still present after cleanup: %v", err)
	}
}
