package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestKernelNaming(t *testing.T) {
	if got := kernelStoredName("202601010000", "x86-64"); got != "simplek8s.202601010000.x86-64.efi" {
		t.Fatalf("kernelStoredName = %q", got)
	}
	if got := kernelArtifactName("202601010000", "x86-64"); got != "simplek8s.202601010000.x86-64.efi.zst" {
		t.Fatalf("kernelArtifactName = %q", got)
	}
	if ts, ok := versionFromStoredKernel("simplek8s.202601010000.x86-64.efi"); !ok || ts != "202601010000" {
		t.Fatalf("versionFromStoredKernel = %q, %v", ts, ok)
	}
	if _, ok := versionFromStoredKernel("simplek8s.202601010000.x86-64.img"); ok {
		t.Fatal("versionFromStoredKernel matched a non-kernel file")
	}
}

func TestListPartitionVersions(t *testing.T) {
	root := t.TempDir()
	dir := "simplek8s"
	for _, ts := range []string{"202601010000", "202603010000", "202602010000"} {
		writeRel(t, root, dir+"/"+kernelStoredName(ts, "x86-64"), "k")
	}
	writeRel(t, root, dir+"/info.json", "{}") // non-kernel, ignored
	got, err := listPartitionVersions(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"202601010000", "202602010000", "202603010000"}
	if len(got) != len(want) {
		t.Fatalf("listPartitionVersions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listPartitionVersions = %v, want %v", got, want)
		}
	}
}

func TestProtectedSetIncludesRunningVersionAndDefault(t *testing.T) {
	root := t.TempDir()
	// Bootloader currently defaults to the 20260201 kernel.
	writeRel(t, root, syslinuxConfigRel, "DEFAULT simplek8s.202602010000.x86-64\n\nLABEL simplek8s.202602010000.x86-64\n KERNEL /simplek8s/simplek8s.202602010000.x86-64.efi\n")
	req := StageRequest{
		Version:    "202603010000",
		Running:    "202601010000",
		Bootloader: BootloaderSyslinux,
	}
	p := protectedSet(req, root)
	for _, ts := range []string{"202601010000", "202602010000", "202603010000"} {
		if !p[ts] {
			t.Fatalf("protectedSet missing %v: %v", ts, p)
		}
	}
}

// TestStagePartitionEndToEnd runs the full pure staging flow against an
// httptest release server: download+verify, zstd extract, copy, bootloader
// entry, and idempotency.
func TestStagePartitionEndToEnd(t *testing.T) {
	const (
		ts   = "202601010000"
		arch = "x86-64"
	)
	payload := []byte("kernel-image-bytes")
	artifact := kernelArtifactName(ts, arch)
	zst := zstdCompress(t, payload)
	sum := sha256.Sum256(zst)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+artifact {
			w.Write(zst)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "TIMEOUT 20\nDEFAULT old\n")
	dir := "simplek8s"
	workDir := t.TempDir()

	req := StageRequest{
		Version:      ts,
		Arch:         arch,
		Checksum:     hex.EncodeToString(sum[:]),
		ArtifactFile: artifact,
		RepoBase:     srv.URL,
		Preserve:     2,
		Running:      "",
		Bootloader:   BootloaderSyslinux,
	}
	if err := stagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, dir, workDir); err != nil {
		t.Fatal(err)
	}

	storedPath := filepath.Join(partRoot, dir, kernelStoredName(ts, arch))
	got, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatalf("stored kernel missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stored kernel content = %q, want %q", got, payload)
	}

	wantKernel := "/" + filepath.Join(dir, kernelStoredName(ts, arch))
	if def := GetBootloaderDefault(BootloaderSyslinux, partRoot); def != wantKernel {
		t.Fatalf("bootloader default = %q, want %q", def, wantKernel)
	}

	// Scratch files are cleaned up.
	if e, _ := os.ReadDir(workDir); len(e) != 0 {
		t.Fatalf("scratch dir not cleaned: %v", e)
	}

	// Idempotency: staging the same version again is a no-op.
	if err := stagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, dir, workDir); err != nil {
		t.Fatalf("second stagePartition: %v", err)
	}
}

// TestStagePartitionGrubEndToEnd mirrors the syslinux end-to-end flow
// on a grub partition: the staged kernel gets a menuentry and the
// named default points at it.
func TestStagePartitionGrubEndToEnd(t *testing.T) {
	const (
		ts   = "202601010000"
		arch = "x86-64"
	)
	payload := []byte("kernel-image-bytes")
	artifact := kernelArtifactName(ts, arch)
	zst := zstdCompress(t, payload)
	sum := sha256.Sum256(zst)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+artifact {
			w.Write(zst)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	partRoot := t.TempDir()
	writeRel(t, partRoot, grubConfigRel, "set default=old\nset timeout=5\n")
	dir := "simplek8s"
	workDir := t.TempDir()

	req := StageRequest{
		Version:      ts,
		Arch:         arch,
		Checksum:     hex.EncodeToString(sum[:]),
		ArtifactFile: artifact,
		RepoBase:     srv.URL,
		Preserve:     2,
		Running:      "",
		Bootloader:   BootloaderGrub,
	}
	if err := stagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, dir, workDir); err != nil {
		t.Fatal(err)
	}

	storedPath := filepath.Join(partRoot, dir, kernelStoredName(ts, arch))
	got, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatalf("stored kernel missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stored kernel content = %q, want %q", got, payload)
	}

	wantKernel := "/" + filepath.Join(dir, kernelStoredName(ts, arch))
	if def := GetBootloaderDefault(BootloaderGrub, partRoot); def != wantKernel {
		t.Fatalf("bootloader default = %q, want %q", def, wantKernel)
	}
}

func TestStagePartitionChecksumMismatchFails(t *testing.T) {	const (
		ts   = "202601010000"
		arch = "x86-64"
	)
	artifact := kernelArtifactName(ts, arch)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zstdCompress(t, []byte("kernel")))
	}))
	defer srv.Close()

	partRoot := t.TempDir()
	writeRel(t, partRoot, syslinuxConfigRel, "DEFAULT old\n")
	dir := "simplek8s"

	req := StageRequest{
		Version:      ts,
		Arch:         arch,
		Checksum:     "wrong",
		ArtifactFile: artifact,
		RepoBase:     srv.URL,
		Bootloader:   BootloaderSyslinux,
	}
	if err := stagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, dir, t.TempDir()); err == nil {
		t.Fatal("stagePartition = no error on checksum mismatch")
	}
	if fileExists(filepath.Join(partRoot, dir, kernelStoredName(ts, arch))) {
		t.Fatal("kernel was written despite checksum mismatch")
	}
}

func TestStagePartitionPurgeKeepsForeignFlavor(t *testing.T) {
	const (
		running = "202603030000"
		newV    = "202604040404"
		arch    = "x86-64"
	)
	payload := []byte("kernel-image-bytes")
	artifact := kernelArtifactName(newV, arch)
	zst := zstdCompress(t, payload)
	sum := sha256.Sum256(zst)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+artifact {
			w.Write(zst)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	partRoot := t.TempDir()
	dir := "simplek8s"
	// Mixed partition: own-flavor old kernels, a newer FOREIGN-flavor
	// file, and the running kernel.
	for _, f := range []string{
		"simplek8s.202601010000.x86-64.efi",
		"simplek8s.202602020000.x86-64.efi",
		"simplek8s.202605050505.rpi4.efi", // foreign AND newest
		"simplek8s." + running + ".x86-64.efi",
	} {
		writeRel(t, partRoot, filepath.Join(dir, f), "old-kernel")
	}
	writeRel(t, partRoot, syslinuxConfigRel,
		"DEFAULT simplek8s."+running+".x86-64\n\nLABEL simplek8s.202601010000.x86-64\n KERNEL /simplek8s/simplek8s.202601010000.x86-64.efi\n")
	workDir := t.TempDir()

	// Force the retention purge regardless of host disk usage: target
	// just above current free.
	total, free, _, err := PathInfo(partRoot)
	if err != nil {
		t.Fatal(err)
	}
	maxUsage := int(100 - (100*(free+1))/total)
	if maxUsage <= 2 || maxUsage >= 100 {
		t.Skipf("cannot force purge on this filesystem (total=%d free=%d)", total, free)
	}
	maxUsage -= 2 // integer-division margin: target must exceed free

	req := StageRequest{
		Version:         newV,
		Arch:            arch,
		Checksum:        hex.EncodeToString(sum[:]),
		ArtifactFile:    artifact,
		RepoBase:        srv.URL,
		Preserve:        0,
		MaxPercentUsage: maxUsage,
		Running:         running,
		Bootloader:      BootloaderSyslinux,
	}
	if err := stagePartition(context.Background(), srv.Client(), discardLogger{}, req, partRoot, dir, workDir); err != nil {
		t.Fatal(err)
	}
	// Foreign file survives despite being newest; unprotected own files
	// are purged (preserve=0 keeps nothing extra); running/staged stay.
	for _, f := range []string{
		"simplek8s.202605050505.rpi4.efi", // foreign: untouched
		"simplek8s." + running + ".x86-64.efi",
		"simplek8s." + newV + ".x86-64.efi",
	} {
		if !fileExists(filepath.Join(partRoot, dir, f)) {
			t.Errorf("file %s missing, want present", f)
		}
	}
	if fileExists(filepath.Join(partRoot, dir, "simplek8s.202601010000.x86-64.efi")) {
		t.Error("unprotected old x86-64 file must be purged")
	}
}
