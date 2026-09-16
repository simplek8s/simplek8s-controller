package update

// Local-node CLI surface (PLAN-M6): the k8s-agnostic operations the
// `simplek8sctl` binary needs, exported for `cmd/simplek8sctl`. The
// controller paths are untouched — every new StageRequest knob
// defaults to the historical behavior, and the extracted purge helper
// preserves the staging sequence byte-for-byte.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Release is one kernel release resolved from the verified index.
type Release struct {
	TS       string // release ts
	Flavor   string // board flavor (x86-64/rpi4/rpi5/legacy arm64)
	Artifact string // compressed artifact filename (.efi.zst)
	Checksum string // sha256 of the artifact (verified index)
	EfiHash  string // sha256 of the bare .efi ("" when the index lacks it)
}

// FilterIndexByFlavor resolves the newest index release for flavor
// (PLAN-M6 D8): the `check.go` inline loop as a pure helper. Only
// compressed kernel artifacts match (ParseKernelRelease); the bare
// `.efi` hash is picked up alongside when the index carries it (the
// PROD repo publishes both).
func FilterIndexByFlavor(sums map[string]string, flavor string) (Release, bool) {
	var r Release
	found := false
	for file := range sums {
		ts, fa, ok := ParseKernelRelease(file)
		if !ok || fa != flavor {
			continue
		}
		if !found || NewerTS(ts, r.TS) {
			r = Release{TS: ts, Flavor: fa, Artifact: file, Checksum: sums[file]}
			found = true
		}
	}
	if !found {
		return Release{}, false
	}
	r.EfiHash = sums[kernelStoredName(r.TS, flavor)]
	return r, true
}

// LookupRelease resolves one explicit ts for flavor from the verified
// index. Absent artifact (or absent checksum) reports ok=false —
// there is nothing verifiable to stage (controller path-2 parity,
// PLAN.md §3.10).
func LookupRelease(sums map[string]string, ts, flavor string) (Release, bool) {
	artifact := kernelArtifactName(ts, flavor)
	checksum, ok := sums[artifact]
	if !ok || checksum == "" {
		return Release{}, false
	}
	return Release{
		TS:       ts,
		Flavor:   flavor,
		Artifact: artifact,
		Checksum: checksum,
		EfiHash:  sums[kernelStoredName(ts, flavor)],
	}, true
}

// DetectArchAuto resolves the node's board flavor without k8s
// (PLAN-M6 D8/D11): staged filenames first (ResolveFlavor), then the
// device-tree (`model`/`compatible` contents), then the build arch —
// `amd64` alone implies `x86-64`, while `arm64` alone cannot
// disambiguate rpi4/rpi5 (generic-`arm64` is unsupported, PLAN.md
// §4.4 D5). Unresolvable reports ok=false: the caller fails closed
// (exit 2), never guesses.
func DetectArchAuto(staged []string, model, compatible, goarch string) (string, bool) {
	if f, ok := ResolveFlavor(staged); ok {
		return f, true
	}
	dt := model + "\x00" + compatible
	if strings.Contains(dt, "Raspberry Pi 5") || strings.Contains(dt, "bcm2712") {
		return "rpi5", true
	}
	if strings.Contains(dt, "Raspberry Pi 4") || strings.Contains(dt, "bcm2711") {
		return "rpi4", true
	}
	if goarch == "amd64" {
		return "x86-64", true
	}
	return "", false
}

// StoredKernelName is the decompressed kernel basename on the
// partition (`simplek8s.<ts>.<flavor>.efi`).
func StoredKernelName(ts, flavor string) string {
	return kernelStoredName(ts, flavor)
}

// LoadKeyringBytes reads an armored or binary keyring from memory
// (the CLI's `go:embed`ded keyring never touches disk).
func LoadKeyringBytes(b []byte) (openpgp.EntityList, error) {
	if el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(b)); err == nil {
		if len(el) == 0 {
			return nil, fmt.Errorf("embedded keyring is empty")
		}
		return el, nil
	}
	if el, err := openpgp.ReadKeyRing(bytes.NewReader(b)); err == nil {
		if len(el) == 0 {
			return nil, fmt.Errorf("embedded keyring is empty")
		}
		return el, nil
	}
	return nil, fmt.Errorf("parse keyring: neither armored nor binary")
}

// ListPartitionVersions lists the release ts staged under
// <partRoot>/<dir> (exported for the CLI's `list` on its own mounted
// session).
func ListPartitionVersions(partRoot, dir string) ([]string, error) {
	return listPartitionVersions(partRoot, dir)
}

// ListPartitionKernels lists staged kernel basenames (with flavor
// part) under <partRoot>/<dir> — the DetectArchAuto input.
func ListPartitionKernels(partRoot, dir string) ([]string, error) {
	entries, err := listKernels(partRoot, dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out, nil
}

// StagePartition stages req onto the already-mounted partition at
// partRoot (exported for the CLI; the controller goes through
// PhysicalStore.Stage).
func StagePartition(ctx context.Context, c *http.Client, log Logger, req StageRequest, partRoot, dir, workDir string) error {
	return stagePartition(ctx, c, log, req, partRoot, dir, workDir)
}

// purgeProtected is the no-delete set for purge flows: the running
// version plus the bootloader default (PLAN.md §3.9 belt-and-braces).
func purgeProtected(running string, bootloader BootloaderType, partRoot string) map[string]bool {
	protected := make(map[string]bool)
	if running != "" {
		protected[running] = true
	}
	if def := GetBootloaderDefault(bootloader, partRoot); def != "" {
		if ts, ok := versionFromStoredKernel(filepath.Base(def)); ok {
			protected[ts] = true
		}
	}
	return protected
}

// PurgePartition applies retention (`preserve`, `maxPercentUsage`
// cap) plus bootloader prune on the already-mounted partition,
// protecting the running version and the bootloader default (PLAN.md
// §3.9 belt-and-braces). It reports the deleted basenames, oldest
// first. A missing kernel dir (fresh partition) is a no-op success.
func PurgePartition(log Logger, partRoot, dir, flavor string, preserve, maxPercent int, running string, bootloader BootloaderType) ([]string, error) {
	return purgeAndPrune(log, partRoot, dir, flavor, preserve, maxPercent, purgeProtected(running, bootloader, partRoot))
}

// PreviewPurge plans retention without writing anything (PLAN-M6
// `purge --dry-run`): same protected set and planning as
// PurgePartition, apply + prune skipped. It reports the basenames
// that would be deleted, oldest first.
func PreviewPurge(log Logger, partRoot, dir, flavor string, preserve, maxPercent int, running string, bootloader BootloaderType) ([]string, error) {
	total, free, _, err := PathInfo(partRoot)
	if err != nil {
		return nil, err
	}
	entries, err := listKernels(partRoot, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return planPurge(ownFlavorEntries(entries, flavor), purgeProtected(running, bootloader, partRoot), preserve, free, targetFreeFromPercent(total, maxPercent)), nil
}

// DefaultLockPath is the shared exclusion lock (PLAN-M6 §3.6): one
// file on the host /run tmpfs, reached in the pod through the
// `hostPath` mount of the dedicated subdir. FD-bound (dies with the
// holder) and reboot-cleared.
const DefaultLockPath = "/run/simplek8s/update.lock"

// ErrBootBusy reports lock contention: another updater (CLI or
// controller) holds the boot-partition session lock.
var ErrBootBusy = errors.New("boot partition session busy (lock held)")

// LockFile takes the non-blocking flock on path (creating dir + file
// as needed). Exclusive for writers, shared for readers. The returned
// func releases; a second contender gets ErrBootBusy.
func LockFile(path string, exclusive bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrBootBusy
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}

// LockShared takes the read lock for mount sessions that write
// nothing (check/list/boot show).
func (s *PhysicalStore) LockShared() (func(), error) {
	return LockFile(s.lockPath, false)
}

// LockExcl takes the write lock for mount sessions that may write
// (controller Stage/EnsureBootGoal, CLI update/purge/boot set).
func (s *PhysicalStore) LockExcl() (func(), error) {
	return LockFile(s.lockPath, true)
}

// FetchVerifiedIndex fetches the release index plus its detached
// signature and verifies the signature (PLAN-M6 check/update
// prelude), returning the verified filename -> sha256 map. With
// keyringPath "/dev/null" verification is skipped (break-glass;
// sha256 of artifacts is still enforced downstream) — any other
// keyringPath overrides the embedded keyring.
func FetchVerifiedIndex(ctx context.Context, c *http.Client, log Logger, baseURL, keyringPath string, embedded []byte) (map[string]string, error) {
	if c == nil {
		c = &http.Client{}
	}
	base := strings.TrimSuffix(baseURL, "/")
	indexBytes, err := httpGet(ctx, c, base+"/"+IndexFile)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", IndexFile, err)
	}
	sigBytes, err := httpGet(ctx, c, base+"/"+IndexSignature)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", IndexSignature, err)
	}
	if keyringPath != "/dev/null" {
		var el openpgp.EntityList
		if keyringPath != "" {
			if el, err = LoadKeyring(keyringPath); err != nil {
				return nil, err
			}
		} else {
			if el, err = LoadKeyringBytes(embedded); err != nil {
				return nil, err
			}
		}
		if _, err := VerifyIndex(el, indexBytes, sigBytes); err != nil {
			return nil, err
		}
	}
	return ParseIndex(indexBytes)
}

// sha256File returns the hex sha256 of a file's bytes.
func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
