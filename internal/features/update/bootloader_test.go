package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRel(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readRel(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetectBootloader(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, syslinuxConfigRel, "DEFAULT old\n")
	writeRel(t, root, rpiConfigRel, "kernel=old\n")
	if got, err := DetectBootloader(root); err != nil || got != BootloaderSyslinux {
		t.Fatalf("DetectBootloader = %q, %v; want syslinux (wins when both)", got, err)
	}

	root2 := t.TempDir()
	writeRel(t, root2, rpiConfigRel, "kernel=old\n")
	if got, err := DetectBootloader(root2); err != nil || got != BootloaderRpi {
		t.Fatalf("DetectBootloader = %q, %v; want rpi", got, err)
	}

	root3 := t.TempDir()
	if _, err := DetectBootloader(root3); err == nil {
		t.Fatal("DetectBootloader = no error, want error (no config)")
	}
}

func TestSyslinuxSetAndGetDefaultReplacesExistingDefault(t *testing.T) {
	root := t.TempDir()
	orig := "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n"
	writeRel(t, root, syslinuxConfigRel, orig)

	newKernel := "simplek8s/simplek8s.202601010000.x86-64.efi"
	if err := SetBootloaderDefault(BootloaderSyslinux, root, newKernel, "/"); err != nil {
		t.Fatal(err)
	}
	got := readRel(t, root, syslinuxConfigRel)

	base := kernelBasename(newKernel)
	if !strings.Contains(got, "DEFAULT "+base) {
		t.Fatalf("config missing DEFAULT %s:\n%s", base, got)
	}
	// The old DEFAULT value is gone; other lines preserved.
	if strings.Contains(got, "DEFAULT old\n") {
		t.Fatalf("old DEFAULT not replaced:\n%s", got)
	}
	if !strings.Contains(got, "TIMEOUT 20\n") || !strings.Contains(got, "LABEL old\n") {
		t.Fatalf("unrelated lines not preserved:\n%s", got)
	}

	if def := GetBootloaderDefault(BootloaderSyslinux, root); def != newKernel {
		t.Fatalf("GetBootloaderDefault = %q, want %q", def, newKernel)
	}
}

func TestSyslinuxSetDefaultAddsMissingLabel(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n")
	newKernel := "simplek8s/simplek8s.202601010000.x86-64.efi"
	if err := SetBootloaderDefault(BootloaderSyslinux, root, newKernel, "/"); err != nil {
		t.Fatal(err)
	}
	got := readRel(t, root, syslinuxConfigRel)
	base := kernelBasename(newKernel)
	if !strings.Contains(got, "LABEL "+base) || !strings.Contains(got, " KERNEL "+newKernel) {
		t.Fatalf("missing LABEL/KERNEL entry:\n%s", got)
	}
	if def := GetBootloaderDefault(BootloaderSyslinux, root); def != newKernel {
		t.Fatalf("GetBootloaderDefault = %q, want %q", def, newKernel)
	}
}

func TestRpiSetAndGetDefault(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, rpiConfigRel, "boot_delay=1\nkernel=old.kernel\n")
	newKernel := "simplek8s/simplek8s.202601010000.aarch64.efi"
	if err := SetBootloaderDefault(BootloaderRpi, root, newKernel, "/"); err != nil {
		t.Fatal(err)
	}
	got := readRel(t, root, rpiConfigRel)
	if !strings.Contains(got, "kernel="+newKernel) || strings.Contains(got, "kernel=old.kernel") {
		t.Fatalf("rpi kernel line not updated:\n%s", got)
	}
	if !strings.Contains(got, "boot_delay=1") {
		t.Fatalf("rpi unrelated line not preserved:\n%s", got)
	}
	if def := GetBootloaderDefault(BootloaderRpi, root); def != newKernel {
		t.Fatalf("GetBootloaderDefault = %q, want %q", def, newKernel)
	}
}

func TestAutoBootloaderResolvesByDetection(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, rpiConfigRel, "kernel=old.kernel\n")
	newKernel := "simplek8s/simplek8s.202601010000.aarch64.efi"
	if err := SetBootloaderDefault(BootloaderAuto, root, newKernel, "/"); err != nil {
		t.Fatal(err)
	}
	if def := GetBootloaderDefault(BootloaderAuto, root); def != newKernel {
		t.Fatalf("GetBootloaderDefault(auto) = %q, want %q", def, newKernel)
	}
}

func TestKernelBasename(t *testing.T) {
	if got := kernelBasename("simplek8s/simplek8s.202601010000.x86-64.efi"); got != "simplek8s.202601010000.x86-64" {
		t.Fatalf("kernelBasename = %q", got)
	}
}
