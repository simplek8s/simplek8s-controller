package update

// Staging orchestration (PLAN-M2 3.7). stagePartition runs on an
// already-mounted partition root (the physical mount/discovery lives in
// PhysicalStore), so the whole flow — idempotency, download + verify,
// zstd extract, capacity pre-check, copy, bootloader entry, purge — is
// pure filesystem work unit-testable in a temp dir.

import (
	"context"
	"errors"
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
	// EfiChecksum is the verified index hash of the bare .efi
	// (PLAN-M6 auto-overwrite). Empty keeps the historical
	// name-only idempotency; set, a same-name file with different
	// bytes is replaced (warned) instead of skipped.
	EfiChecksum string
	// NoRepoint stages without re-pointing the bootloader default
	// (PLAN-M6 `update --next-kernel=false`). Default false keeps
	// the controller behavior (always re-point).
	NoRepoint bool
	// DryRun prints the plan (download to scratch for exact sizing)
	// and touches nothing on the partition.
	DryRun bool
}

// storedKernelRe matches a decompressed kernel name on the partition:
// simplek8s.<ts>.<arch>.efi. The .efi file is the kernel+initrd image (it
// boots both BIOS and UEFI); .kernel was the pre-2024 legacy name and is no
// longer produced.
var storedKernelRe = regexp.MustCompile(`^simplek8s\.([0-9]+)\.([A-Za-z0-9_-]+)\.efi$`)

// versionFromStoredKernel parses a stored kernel basename into its ts.
func versionFromStoredKernel(filename string) (string, bool) {
	m := storedKernelRe.FindStringSubmatch(filename)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// kernelStoredName is the decompressed kernel filename on the partition
// (the .efi image: kernel + initrd).
func kernelStoredName(ts, arch string) string {
	return "simplek8s." + ts + "." + arch + ".efi"
}

// kernelArtifactName is the compressed artifact filename in the repo
// (zstd of the .efi image).
func kernelArtifactName(ts, arch string) string {
	return "simplek8s." + ts + "." + arch + ".efi.zst"
}

// ownFlavorEntries drops kernels whose arch differs from the staging
// flavor (PLAN-M5 §3.4: purge never touches foreign-flavor files).
func ownFlavorEntries(entries []kernelEntry, flavor string) []kernelEntry {
	out := make([]kernelEntry, 0, len(entries))
	for _, e := range entries {
		if _, fa, ok := ParseStoredKernel(e.name); ok && fa == flavor {
			out = append(out, e)
		}
	}
	return out
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
// log reports the significant steps (download/verify, extract, write);
// pass discardLogger for pure unit tests.
func stagePartition(ctx context.Context, c *http.Client, log Logger, req StageRequest, partRoot, dir, workDir string) error {
	if c == nil {
		c = &http.Client{}
	}
	stored := kernelStoredName(req.Version, req.Arch)
	storedPath := filepath.Join(partRoot, dir, stored)

	// 1. idempotency: already staged -> nothing to do. With an
	// EfiChecksum (CLI path) the gate is hash-based: same bytes
	// skip, same name with different bytes fall through to a warned
	// replace. Without it the historical name-only skip stands
	// (controller path, unchanged).
	if fileExists(storedPath) {
		if req.EfiChecksum == "" {
			log.Debug("update: kernel already staged, skipping", "version", req.Version, "stored", stored)
			return nil
		}
		if sum, serr := sha256File(storedPath); serr == nil && strings.EqualFold(sum, req.EfiChecksum) {
			log.Debug("update: kernel already staged (hash-verified), skipping", "version", req.Version, "stored", stored)
			return nil
		}
		log.Warn("update: staged file differs from verified hash; replacing", "version", req.Version, "stored", stored)
	}
	log.Debug("update: staging release", "version", req.Version, "stored", stored)

	// 2. download the artifact and verify its sha256 (verified index).
	artifact := req.ArtifactFile
	if artifact == "" {
		artifact = kernelArtifactName(req.Version, req.Arch)
	}
	artPath := filepath.Join(workDir, artifact)
	url := strings.TrimSuffix(req.RepoBase, "/") + "/" + artifact
	if err := downloadAndVerify(ctx, c, url, req.Checksum, artPath); err != nil {
		os.Remove(artPath)
		log.Warn("update: kernel download/verify failed", "version", req.Version, "artifact", artifact, "err", err)
		return fmt.Errorf("download %s: %w", artifact, err)
	}
	var dlBytes int64
	if st, serr := os.Stat(artPath); serr == nil {
		dlBytes = st.Size()
	}
	log.Info("update: kernel downloaded", "version", req.Version, "artifact", artifact, "bytes", dlBytes)
	defer os.Remove(artPath)

	// 3. extract the kernel to the scratch dir.
	extPath := filepath.Join(workDir, stored)
	if err := extractZstd(artPath, extPath); err != nil {
		os.Remove(extPath)
		log.Warn("update: kernel extract failed", "version", req.Version, "artifact", artifact, "err", err)
		return fmt.Errorf("extract %s: %w", artifact, err)
	}
	defer os.Remove(extPath)
	extSize, err := statSize(extPath)
	if err != nil {
		return err
	}
	log.Debug("update: kernel extracted", "version", req.Version, "stored", stored, "bytes", extSize)

	// 3b. decompression check (CLI path): the extracted bytes must
	// match the verified bare-.efi hash — integrity beyond the zstd
	// frame checksum. Skipped without EfiChecksum (controller path).
	if req.EfiChecksum != "" {
		sum, serr := sha256File(extPath)
		if serr != nil {
			return serr
		}
		if !strings.EqualFold(sum, req.EfiChecksum) {
			os.Remove(extPath)
			return fmt.Errorf("extracted %s mismatch vs verified hash", stored)
		}
	}

	// 3c. dry run: exact plan, zero partition writes (the download +
	// extract above went to scratch, off-partition).
	if req.DryRun {
		return stageDryRun(log, req, partRoot, dir, stored, extSize)
	}

	protected := protectedSet(req, partRoot)

	// Track purge deletions for the bootloader prune below (§3.9).
	var purged []string
	purge := func(names []string) error {
		if err := applyPurge(partRoot, dir, names); err != nil {
			return err
		}
		purged = append(purged, names...)
		return nil
	}

	// 4. capacity pre-check: make room for the new kernel by purging the
	// oldest unprotected versions.
	_, free, _, err := PathInfo(partRoot)
	if err != nil {
		return err
	}
	if free < uint64(extSize) {
		entries, lerr := listKernels(partRoot, dir)
		if lerr != nil {
			return lerr
		}
		toDelete := planPurge(ownFlavorEntries(entries, req.Arch), protected, 0, free, uint64(extSize))
		if err := purge(toDelete); err != nil {
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
	log.Debug("update: kernel written to boot partition", "version", req.Version, "stored", stored)

	// 6. point the bootloader default at the new kernel. The path is rooted
	// at the boot-partition mount point (leading "/"), matching the
	// reference syslinux/rpi layout. NoRepoint (CLI
	// `--next-kernel=false`) stages the file and stops here.
	if req.NoRepoint {
		log.Debug("update: leaving bootloader default untouched", "version", req.Version, "stored", stored)
	} else if err := SetBootloaderDefault(req.Bootloader, partRoot, "/"+filepath.Join(dir, stored), "/"); err != nil {
		return fmt.Errorf("bootloader: %w", err)
	} else {
		log.Debug("update: bootloader default updated", "version", req.Version, "target", "/"+filepath.Join(dir, stored))
	}

	// 7. retention purge: keep the newest `preserve`, enforce the usage cap.
	// 8. bootloader stale-entry prune (PLAN.md §3.9): only when a purge
	// deleted at least one kernel, in this same mounted session, after
	// the staging work. Grub and syslinux-legacy partitions prune
	// their own config; rpi has nothing to prune. A prune failure
	// never fails staging (Warn + retry on the next purge-triggered
	// session).
	if _, err := purgeAndPrune(log, partRoot, dir, req.Arch, req.Preserve, req.MaxPercentUsage, protected); err != nil {
		return err
	}
	return nil
}

// stageDryRun reports the exact staging plan without touching the
// partition (PLAN-M6 `--dry-run`): the artifact was already
// downloaded + extracted to scratch, so sizes and purge previews are
// exact. Read-only on the partition (statfs + listings).
func stageDryRun(log Logger, req StageRequest, partRoot, dir, stored string, extSize int64) error {
	total, free, _, err := PathInfo(partRoot)
	if err != nil {
		return err
	}
	entries, lerr := listKernels(partRoot, dir)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			entries = nil // fresh partition: nothing staged yet
		} else {
			return lerr
		}
	}
	protected := protectedSet(req, partRoot)
	own := ownFlavorEntries(entries, req.Arch)
	room := planPurge(own, protected, 0, free, uint64(extSize))
	retention := planPurge(own, protected, req.Preserve, free, targetFreeFromPercent(total, req.MaxPercentUsage))
	if req.NoRepoint {
		log.Info("update: dry-run: would stage without re-pointing default", "stored", stored, "bytes", extSize)
	} else {
		log.Info("update: dry-run: would stage and re-point default", "stored", stored, "bytes", extSize)
	}
	if len(room) > 0 {
		log.Info("update: dry-run: would purge for room", "delete", room)
	}
	if len(retention) > 0 {
		log.Info("update: dry-run: would purge for retention", "delete", retention)
	}
	if len(room) > 0 || len(retention) > 0 {
		log.Info("update: dry-run: would prune bootloader entries", "flavor", req.Arch)
	}
	return nil
}
