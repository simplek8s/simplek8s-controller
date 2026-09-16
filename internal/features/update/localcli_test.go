package update

// Unit tests for the local-node CLI surface (PLAN-M6 §6.2): the pure
// release/arch helpers, the staging knobs (NoRepoint/DryRun/hash
// gate), PurgePartition and the shared lock.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFilterIndexByFlavor(t *testing.T) {
	sums := map[string]string{
		"simplek8s.202601010000.x86-64.efi.zst": "aa",
		"simplek8s.202602020000.x86-64.efi.zst": "bb",
		"simplek8s.202602020000.x86-64.efi":     "cc",
		"simplek8s.202603030000.rpi4.efi.zst":   "dd",
		"simplek8s.202603030000.rpi4.efi":       "ee",
		"simplek8s.latest.x86-64.efi.zst":       "ff",
		"simplek8s.202602020000.x86-64.img.zst": "gg",
		"SHA256SUMS.gpg":                        "hh",
	}
	r, ok := FilterIndexByFlavor(sums, "x86-64")
	if !ok {
		t.Fatal("want match for x86-64")
	}
	if r.TS != "202602020000" || r.Artifact != "simplek8s.202602020000.x86-64.efi.zst" || r.Checksum != "bb" {
		t.Fatalf("release = %+v", r)
	}
	if r.EfiHash != "cc" {
		t.Fatalf("EfiHash = %q, want cc", r.EfiHash)
	}
	if _, ok := FilterIndexByFlavor(sums, "rpi5"); ok {
		t.Fatal("want no match for rpi5")
	}
	r4, ok := FilterIndexByFlavor(sums, "rpi4")
	if !ok || r4.TS != "202603030000" || r4.EfiHash != "ee" {
		t.Fatalf("rpi4 release = %+v, %v", r4, ok)
	}
}

func TestLookupRelease(t *testing.T) {
	sums := map[string]string{
		"simplek8s.202601010000.x86-64.efi.zst": "aa",
		"simplek8s.202601010000.x86-64.efi":     "bb",
	}
	r, ok := LookupRelease(sums, "202601010000", "x86-64")
	if !ok || r.Checksum != "aa" || r.EfiHash != "bb" || r.Artifact != "simplek8s.202601010000.x86-64.efi.zst" {
		t.Fatalf("release = %+v, %v", r, ok)
	}
	if _, ok := LookupRelease(sums, "199901010000", "x86-64"); ok {
		t.Fatal("want no match for absent ts")
	}
	if _, ok := LookupRelease(sums, "202601010000", "rpi4"); ok {
		t.Fatal("want no match for foreign flavor")
	}
}

func TestDetectArchAuto(t *testing.T) {
	staged := []string{"simplek8s.202601010000.rpi4.efi"}
	if f, ok := DetectArchAuto(staged, "", "", "amd64"); !ok || f != "rpi4" {
		t.Fatalf("staged-first = %q, %v", f, ok)
	}
	if f, ok := DetectArchAuto(nil, "Raspberry Pi 5 Model B", "", "arm64"); !ok || f != "rpi5" {
		t.Fatalf("model rpi5 = %q, %v", f, ok)
	}
	if f, ok := DetectArchAuto(nil, "Raspberry Pi 4 Model B", "", "arm64"); !ok || f != "rpi4" {
		t.Fatalf("model rpi4 = %q, %v", f, ok)
	}
	if f, ok := DetectArchAuto(nil, "", "brcm,bcm2712", "arm64"); !ok || f != "rpi5" {
		t.Fatalf("compat bcm2712 = %q, %v", f, ok)
	}
	if f, ok := DetectArchAuto(nil, "", "brcm,bcm2711", "arm64"); !ok || f != "rpi4" {
		t.Fatalf("compat bcm2711 = %q, %v", f, ok)
	}
	if f, ok := DetectArchAuto(nil, "", "", "amd64"); !ok || f != "x86-64" {
		t.Fatalf("amd64 machine = %q, %v", f, ok)
	}
	// arm64 alone cannot disambiguate (generic-arm64 unsupported).
	if _, ok := DetectArchAuto(nil, "", "", "arm64"); ok {
		t.Fatal("want unresolvable for bare arm64")
	}
	if _, ok := DetectArchAuto([]string{"README", "simplek8s.latest.x86-64.efi"}, "", "", "arm64"); ok {
		t.Fatal("want unresolvable for foreign-only staged")
	}
}

func TestLoadKeyringBytes(t *testing.T) {
	k := newTestKey(t, true)
	raw, err := os.ReadFile(k.keyring)
	if err != nil {
		t.Fatal(err)
	}
	el, err := LoadKeyringBytes(raw)
	if err != nil {
		t.Fatalf("armored bytes: %v", err)
	}
	if len(el) != 1 {
		t.Fatalf("%d entities", len(el))
	}
	kb := newTestKey(t, false)
	rawB, err := os.ReadFile(kb.keyring)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyringBytes(rawB); err != nil {
		t.Fatalf("binary bytes: %v", err)
	}
	if _, err := LoadKeyringBytes([]byte("not a keyring")); err == nil {
		t.Fatal("want error for garbage")
	}
}

// cliStageFixture serves one zstd artifact and returns its server,
// payload and checksums.
func cliStageFixture(t *testing.T, ts, arch string, payload []byte) (*httptest.Server, string, string, string) {
	t.Helper()
	artifact := kernelArtifactName(ts, arch)
	zst := zstdCompress(t, payload)
	zsum := sha256.Sum256(zst)
	psum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+artifact {
			w.Write(zst)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, artifact, hex.EncodeToString(zsum[:]), hex.EncodeToString(psum[:])
}

func TestStageNoRepoint(t *testing.T) {
	const ts, arch = "202601010000", "x86-64"
	payload := []byte("kernel-bytes")
	srv, artifact, zsum, psum := cliStageFixture(t, ts, arch, payload)
	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n")
	req := StageRequest{
		Version: ts, Arch: arch, Checksum: zsum, ArtifactFile: artifact,
		RepoBase: srv.URL, Preserve: 3, Bootloader: BootloaderSyslinux,
		EfiChecksum: psum, NoRepoint: true,
	}
	if err := StagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, "simplek8s", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(partRoot, "simplek8s", kernelStoredName(ts, arch))); string(got) != string(payload) {
		t.Fatalf("staged content = %q", got)
	}
	if def := GetBootloaderDefault(BootloaderSyslinux, partRoot); def != "old.kernel" {
		t.Fatalf("default = %q, want untouched old.kernel", def)
	}
}

func TestStageDryRunWritesNothing(t *testing.T) {
	const ts, arch = "202601010000", "x86-64"
	payload := []byte("kernel-bytes")
	srv, artifact, zsum, psum := cliStageFixture(t, ts, arch, payload)
	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n")
	req := StageRequest{
		Version: ts, Arch: arch, Checksum: zsum, ArtifactFile: artifact,
		RepoBase: srv.URL, Preserve: 3, Bootloader: BootloaderSyslinux,
		EfiChecksum: psum, DryRun: true,
	}
	if err := StagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, "simplek8s", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(partRoot, "simplek8s")); !os.IsNotExist(err) {
		t.Fatal("dry-run created the kernel dir")
	}
	if def := GetBootloaderDefault(BootloaderSyslinux, partRoot); def != "old.kernel" {
		t.Fatalf("dry-run moved default to %q", def)
	}
}

func TestStageHashGateSkipsWithoutNetwork(t *testing.T) {
	const ts, arch = "202601010000", "x86-64"
	payload := []byte("kernel-bytes")
	psum := sha256.Sum256(payload)
	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n")
	writeRel(t, partRoot, "simplek8s/"+kernelStoredName(ts, arch), string(payload))
	// Unreachable repo: identical staged bytes must skip before dialing.
	req := StageRequest{
		Version: ts, Arch: arch, Checksum: "00", ArtifactFile: kernelArtifactName(ts, arch),
		RepoBase: "http://127.0.0.1:1", Preserve: 3, Bootloader: BootloaderSyslinux,
		EfiChecksum: hex.EncodeToString(psum[:]),
	}
	if err := StagePartition(context.Background(), nil, discardLogger{}, req, partRoot, "simplek8s", t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestStageHashGateReplacesDiffering(t *testing.T) {
	const ts, arch = "202601010000", "x86-64"
	payload := []byte("kernel-bytes")
	srv, artifact, zsum, psum := cliStageFixture(t, ts, arch, payload)
	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n")
	writeRel(t, partRoot, "simplek8s/"+kernelStoredName(ts, arch), "stale-corrupt-bytes")
	req := StageRequest{
		Version: ts, Arch: arch, Checksum: zsum, ArtifactFile: artifact,
		RepoBase: srv.URL, Preserve: 3, Bootloader: BootloaderSyslinux,
		EfiChecksum: psum,
	}
	if err := StagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, "simplek8s", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(partRoot, "simplek8s", kernelStoredName(ts, arch))); string(got) != string(payload) {
		t.Fatalf("staged content = %q, want replaced", got)
	}
}

func TestStageDecompressionMismatchFails(t *testing.T) {
	const ts, arch = "202601010000", "x86-64"
	payload := []byte("kernel-bytes")
	srv, artifact, zsum, _ := cliStageFixture(t, ts, arch, payload)
	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n\nLABEL old\n KERNEL old.kernel\n")
	req := StageRequest{
		Version: ts, Arch: arch, Checksum: zsum, ArtifactFile: artifact,
		RepoBase: srv.URL, Preserve: 3, Bootloader: BootloaderSyslinux,
		EfiChecksum: "ff" + zsum[2:],
	}
	if err := StagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, "simplek8s", t.TempDir()); err == nil {
		t.Fatal("want error for extracted-vs-index mismatch")
	}
	if _, err := os.Stat(filepath.Join(partRoot, "simplek8s")); !os.IsNotExist(err) {
		t.Fatal("failed stage wrote to the partition")
	}
}

func TestPurgePartition(t *testing.T) {
	root := t.TempDir()
	dir := "simplek8s"
	for _, ts := range []string{"202601010000", "202602020000", "202603030000", "202604040000", "202605050000"} {
		writeRel(t, root, dir+"/"+kernelStoredName(ts, "x86-64"), "k")
	}
	// Running is old, default points at the second-oldest: both survive.
	writeRel(t, root, syslinuxConfigRel, "DEFAULT simplek8s.202602020000.x86-64\n\nLABEL simplek8s.202602020000.x86-64\n KERNEL /simplek8s/simplek8s.202602020000.x86-64.efi\n")
	deleted, err := PurgePartition(discardLogger{}, root, dir, "x86-64", 3, 75, "202601010000", BootloaderSyslinux)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		// preserve=3 keeps the 3 newest non-protected; running(01)
		// and default(02) are protected; 03,04,05 stay.
		t.Fatalf("deleted = %v, want none", deleted)
	}
	// Retention deletion under a usage cap cannot trigger on a roomy
	// filesystem (planPurge only deletes while free < target); the
	// deletion + protection planning is unit-covered
	// (TestPlanPurgeDeletesOldestFirst,
	// TestPlanPurgeNeverDeletesProtected) and the apply is covered
	// (TestApplyPurgeRemovesOnlyNamedKernels). Here: all staged
	// files survive the no-op.
	for _, ts := range []string{"202601010000", "202602020000", "202603030000", "202604040000", "202605050000"} {
		if _, err := os.Stat(filepath.Join(root, dir, kernelStoredName(ts, "x86-64"))); err != nil {
			t.Fatalf("staged %s removed by no-op purge", ts)
		}
	}
	// Missing kernel dir is a no-op success.
	if _, err := PurgePartition(discardLogger{}, t.TempDir(), dir, "x86-64", 3, 75, "", BootloaderAuto); err != nil {
		t.Fatalf("empty partition: %v", err)
	}
}

func TestLockFileContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	un1, err := LockFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockFile(path, true); !errors.Is(err, ErrBootBusy) {
		t.Fatalf("second excl = %v, want ErrBootBusy", err)
	}
	if _, err := LockFile(path, false); !errors.Is(err, ErrBootBusy) {
		t.Fatalf("shared vs excl = %v, want ErrBootBusy", err)
	}
	un1()
	un2, err := LockFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	un3, err := LockFile(path, false)
	if err != nil {
		t.Fatalf("shared+shared: %v", err)
	}
	un2()
	un3()
	if _, err := LockFile(path, true); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
}
