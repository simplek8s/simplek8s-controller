package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// newTestStore builds a store with injected seams over a temp /dev root.
func newTestStore(t *testing.T, blkid func(args ...string) (string, error)) *PhysicalStore {
	t.Helper()
	devRoot := t.TempDir()
	mountRoot := t.TempDir()
	return NewPhysicalStore(PhysicalStoreConfig{
		DevRoot:   devRoot,
		MountRoot: mountRoot,
		Blkid:     blkid,
		Log:       discardLogger{},
	})
}

func TestFindBootDeviceByLabelSymlink(t *testing.T) {
	s := newTestStore(t, nil)
	labelDir := filepath.Join(s.devRoot, "disk", "by-label")
	if err := os.MkdirAll(labelDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(s.devRoot, "nvme0n1p2")
	if err := os.Symlink(target, filepath.Join(labelDir, "EFI")); err != nil {
		t.Fatal(err)
	}
	dev, err := s.findBootDevice(context.Background())
	if err != nil {
		t.Fatalf("findBootDevice: %v", err)
	}
	if filepath.Base(dev) != "EFI" {
		t.Fatalf("findBootDevice = %q, want the by-label EFI symlink", dev)
	}
}

func TestFindBootDeviceBlkidFallback(t *testing.T) {
	called := false
	s := newTestStore(t, func(args ...string) (string, error) {
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
}

func TestFindBootDeviceNone(t *testing.T) {
	s := newTestStore(t, func(args ...string) (string, error) {
		return "", nil
	})
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error, want an error (no labeled device)")
	}
}

func TestFindBootDeviceBlkidError(t *testing.T) {
	s := newTestStore(t, func(args ...string) (string, error) {
		return "", errors.New("blkid not found")
	})
	if _, err := s.findBootDevice(context.Background()); err == nil {
		t.Fatal("findBootDevice = no error, want an error (blkid failed)")
	}
}
