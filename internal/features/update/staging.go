package update

// Staging orchestration (PLAN-M2 3.7). stagePartition runs on an
// already-mounted partition root (the physical mount/discovery lives in
// PhysicalStore), so the whole flow — idempotency, download + verify,
// zstd extract, capacity pre-check, copy, bootloader entry, purge — is
// pure filesystem work unit-testable in a temp dir.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// StageRequest carries everything needed to stage one release onto this
// node's boot partition.
type StageRequest struct {
	Version         string // release ts
	Arch            string // release arch (x86-64 / aarch64)
	Checksum        string // sha256 of the .zst artifact (verified index)
	ArtifactFile    string // artifact filename in the repo
	RepoBase        string // repo base URL
	Preserve        int    // updates.preserve
	MaxPercentUsage int    // updates.max-percent-usage
	Running         string // running ts (never deleted)
	Bootloader      BootloaderType
}

// storedKernelRe matches a decompressed kernel name on the partition:
// simplek8s.<ts>.<arch>.kernel.
var storedKernelRe = regexp.MustCompile(`^simplek8s\.([0-9]+)\.([A-Za-z0-9_-]+)\.kernel$`)

// versionFromStoredKernel parses a stored kernel basename into its ts.
func versionFromStoredKernel(filename string) (string, bool) {
	m := storedKernelRe.FindStringSubmatch(filename)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// kernelStoredName is the decompressed kernel filename on the partition.
func kernelStoredName(ts, arch string) string {
	return "simplek8s." + ts + "." + arch + ".kernel"
}

// kernelArtifactName is the compressed artifact filename in the repo.
func kernelArtifactName(ts, arch string) string {
	return "simplek8s." + ts + "." + arch + ".kernel.zst"
}

// listPartitionVersions returns the release ts present under
// <partRoot>/<dir>, sorted ascending.
func listPartitionVersions(partRoot, dir string) ([]string, error) {
	entries, err := listKernels(partRoot, dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ts)
	}
	sort.Slice(out, func(i, j int) bool { return NewerTS(out[j], out[i]) })
	return out, nil
}

// protectedSet is the set of ts that must never be purged: the running
// version, the version being staged (it becomes the new default), and the
// current bootloader default.
func protectedSet(req StageRequest, partRoot string) map[string]bool {
	s := make(map[string]bool)
	if req.Running != "" {
		s[req.Running] = true
	}
	if req.Version != "" {
		s[req.Version] = true
	}
	if def := GetBootloaderDefault(req.Bootloader, partRoot); def != "" {
		if ts, ok := versionFromStoredKernel(filepath.Base(def)); ok {
			s[ts] = true
		}
	}
	return s
}

// stagePartition stages req onto the mounted partition at partRoot.
// workDir is a scratch dir (pod temp) for the download + extracted kernel.
func stagePartition(ctx context.Context, c *http.Client, req StageRequest, partRoot, dir, workDir string) error {
	if c == nil {
		c = &http.Client{}
	}
	stored := kernelStoredName(req.Version, req.Arch)
	storedPath := filepath.Join(partRoot, dir, stored)

	// 1. idempotency: already staged -> nothing to do.
	if fileExists(storedPath) {
		return nil
	}

	// 2. download the artifact and verify its sha256 (verified index).
	artifact := req.ArtifactFile
	if artifact == "" {
		artifact = kernelArtifactName(req.Version, req.Arch)
	}
	artPath := filepath.Join(workDir, artifact)
	url := strings.TrimSuffix(req.RepoBase, "/") + "/" + artifact
	if err := downloadAndVerify(ctx, c, url, req.Checksum, artPath); err != nil {
		os.Remove(artPath)
		return fmt.Errorf("download %s: %w", artifact, err)
	}
	defer os.Remove(artPath)

	// 3. extract the kernel to the scratch dir.
	extPath := filepath.Join(workDir, stored)
	if err := extractZstd(artPath, extPath); err != nil {
		os.Remove(extPath)
		return fmt.Errorf("extract %s: %w", artifact, err)
	}
	defer os.Remove(extPath)
	extSize, err := statSize(extPath)
	if err != nil {
		return err
	}

	protected := protectedSet(req, partRoot)

	// 4. capacity pre-check: make room for the new kernel by purging the
	// oldest unprotected versions.
	total, free, _, err := PathInfo(partRoot)
	if err != nil {
		return err
	}
	if free < uint64(extSize) {
		entries, lerr := listKernels(partRoot, dir)
		if lerr != nil {
			return lerr
		}
		toDelete := planPurge(entries, protected, 0, free, uint64(extSize))
		if err := applyPurge(partRoot, dir, toDelete); err != nil {
			return err
		}
		if _, free, _, err = PathInfo(partRoot); err != nil {
			return err
		}
		if free < uint64(extSize) {
			return fmt.Errorf("not enough space on boot partition for %s: free %d, need %d", stored, free, extSize)
		}
	}

	// 5. copy the kernel onto the partition.
	if err := os.MkdirAll(filepath.Join(partRoot, dir), 0755); err != nil {
		return err
	}
	if err := copyFile(extPath, storedPath); err != nil {
		return fmt.Errorf("write %s: %w", stored, err)
	}

	// 6. point the bootloader default at the new kernel.
	if err := SetBootloaderDefault(req.Bootloader, partRoot, filepath.Join(dir, stored), "/"); err != nil {
		return fmt.Errorf("bootloader: %w", err)
	}

	// 7. retention purge: keep the newest `preserve`, enforce the usage cap.
	_, free, _, err = PathInfo(partRoot)
	if err != nil {
		return err
	}
	entries, lerr := listKernels(partRoot, dir)
	if lerr != nil {
		return lerr
	}
	target := targetFreeFromPercent(total, req.MaxPercentUsage)
	return applyPurge(partRoot, dir, planPurge(entries, protected, req.Preserve, free, target))
}
