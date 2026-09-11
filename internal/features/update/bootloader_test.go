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

// PLAN.md §3.9 reference fixture: the distro's initial entry plus one
// staged by the controller.
const pruneSample = `DEFAULT simplek8s.202609061935.x86-64

LABEL simplek8s.202608291203.x86-64
 KERNEL /simplek8s/simplek8s.202608291203.x86-64.efi
 #APPEND log_buf_len=5M printk.devkmsg=on systemd.debug-shell=1 debug

LABEL simplek8s.202609061935.x86-64
 KERNEL /simplek8s/simplek8s.202609061935.x86-64.efi

`

func TestPruneSyslinuxEntries(t *testing.T) {
	gone := func(base string) bool {
		return base == "simplek8s.202608291203.x86-64.efi"
	}
	got, n := pruneSyslinuxEntries(pruneSample, "simplek8s.202609061935.x86-64", "x86-64", gone)
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	want := `DEFAULT simplek8s.202609061935.x86-64

LABEL simplek8s.202609061935.x86-64
 KERNEL /simplek8s/simplek8s.202609061935.x86-64.efi

`
	if got != want {
		t.Fatalf("rewritten config:\n%q\nwant:\n%q", got, want)
	}
}

func TestPruneSyslinuxEntriesGuards(t *testing.T) {
	never := func(string) bool { return false }
	// Nothing gone: byte-identical, including the DOC header.
	doc := "# distro documentation header\n# kept verbatim\n" + pruneSample
	if got, n := pruneSyslinuxEntries(doc, "simplek8s.202609061935.x86-64", "x86-64", never); n != 0 || got != doc {
		t.Fatalf("no-op rewrite: n=%d identical=%v", n, got == doc)
	}
	// DEFAULT block is never pruned even when its file is gone.
	always := func(string) bool { return true }
	got, n := pruneSyslinuxEntries(pruneSample, "simplek8s.202609061935.x86-64", "x86-64", always)
	if n != 1 {
		t.Fatalf("pruned = %d, want 1 (DEFAULT spared)", n)
	}
	if !strings.Contains(got, "LABEL simplek8s.202609061935.x86-64") {
		t.Fatal("DEFAULT block must survive")
	}
	// Foreign entries are never touched, file present or not.
	foreign := "DEFAULT recovery\n\nLABEL recovery\n KERNEL /recovery/vmlinuz\n\nLABEL ours\n KERNEL /simplek8s/simplek8s.1.x86-64.efi\n"
	got, n = pruneSyslinuxEntries(foreign, "recovery", "x86-64", always)
	if n != 1 {
		t.Fatalf("pruned = %d, want 1 (foreign spared)", n)
	}
	if !strings.Contains(got, "LABEL recovery") {
		t.Fatal("foreign block must survive")
	}
	// Foreign-FLAVOR entries are never touched either (PLAN-M5 §3.4).
	otherarch := "DEFAULT ours\n\nLABEL rpi\n KERNEL /simplek8s/simplek8s.1.rpi4.efi\n\nLABEL ours\n KERNEL /simplek8s/simplek8s.1.x86-64.efi\n"
	if _, n := pruneSyslinuxEntries(otherarch, "other", "x86-64", always); n != 1 {
		t.Fatalf("pruned = %d, want 1 (foreign flavor spared, ours pruned)", n)
	}
	// Blocks without a KERNEL line are never touched.
	nokern := "DEFAULT ours\n\nLABEL odd\n APPEND console=ttyS0\n\nLABEL ours\n KERNEL /simplek8s/simplek8s.1.x86-64.efi\n"
	if _, n := pruneSyslinuxEntries(nokern, "other", "x86-64", always); n != 1 {
		t.Fatalf("pruned = %d, want 1 (KERNEL-less spared, ours pruned)", n)
	}
}

func TestPruneSyslinuxFileEndToEnd(t *testing.T) {
	root := t.TempDir()
	dir := "simplek8s"
	writeRel(t, root, "syslinux/syslinux.cfg", pruneSample)
	// Only the old kernel file is gone; the default's file exists.
	writeRel(t, root, "simplek8s/simplek8s.202609061935.x86-64.efi", "kernel")
	pruneSyslinuxFile(root, dir, "x86-64", discardLogger{})
	cfg := readRel(t, root, "syslinux/syslinux.cfg")
	if strings.Contains(cfg, "LABEL simplek8s.202608291203.x86-64") {
		t.Fatal("stale entry must be pruned")
	}
	if !strings.Contains(cfg, "LABEL simplek8s.202609061935.x86-64") {
		t.Fatal("DEFAULT entry must survive")
	}
	// Non-syslinux partition (no config): silent no-op.
	pruneSyslinuxFile(t.TempDir(), dir, "x86-64", discardLogger{})
}
