package updatecore

// Boot-partition purge (PLAN-M2 3.7 step 6): keep the newest
// `updates.preserve` versions, then enforce usage <=
// `updates.max-percent-usage`; the running version is NEVER deleted.
// The planning is pure (unit-tested); the apply removes the chosen
// files on the mounted partition.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// kernelEntry is one stored kernel on the partition.
type kernelEntry struct {
	ts   string
	name string // basename, e.g. simplek8s.<ts>.<arch>.efi
	size int64
}

// listKernels reads the stored kernels under <partRoot>/<dir>.
func listKernels(partRoot, dir string) ([]kernelEntry, error) {
	path := filepath.Join(partRoot, dir)
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []kernelEntry
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		ts, ok := versionFromStoredKernel(name)
		if !ok {
			continue
		}
		sz, _ := statSize(filepath.Join(path, name))
		out = append(out, kernelEntry{ts: ts, name: name, size: sz})
	}
	return out, nil
}

// planPurge returns the file names to delete (oldest first) so that,
// after deletion, free >= targetFree, while never deleting a protected
// ts and always keeping the newest keepNewest non-protected versions.
// It returns nil when free already meets targetFree.
func planPurge(entries []kernelEntry, protected map[string]bool, keepNewest int, free, targetFree uint64) []string {
	if free >= targetFree {
		return nil
	}
	// keep the newest keepNewest (by ts) non-protected versions
	keep := make(map[string]bool)
	desc := append([]kernelEntry{}, entries...)
	sort.Slice(desc, func(i, j int) bool { return NewerTS(desc[i].ts, desc[j].ts) })
	for i, e := range desc {
		if i >= keepNewest {
			break
		}
		if !protected[e.ts] {
			keep[e.ts] = true
		}
	}
	// walk oldest first, deleting unprotected, non-kept until satisfied
	asc := append([]kernelEntry{}, entries...)
	sort.Slice(asc, func(i, j int) bool { return NewerTS(asc[j].ts, asc[i].ts) })
	var del []string
	cur := free
	for _, e := range asc {
		if cur >= targetFree {
			break
		}
		if protected[e.ts] || keep[e.ts] {
			continue
		}
		del = append(del, e.name)
		cur += uint64(e.size)
	}
	return del
}

// applyPurge removes the named kernels from <partRoot>/<dir>.
func applyPurge(partRoot, dir string, names []string) error {
	for _, n := range names {
		if err := os.Remove(filepath.Join(partRoot, dir, n)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", n, err)
		}
	}
	return nil
}

// targetFreeFromPercent computes the free-space target (bytes) that keeps
// partition usage at or below maxPercent of total. A maxPercent <= 0 or
// >= 100 means "no usage cap".
func targetFreeFromPercent(total uint64, maxPercent int) uint64 {
	if maxPercent <= 0 || maxPercent >= 100 {
		return 0
	}
	return total * uint64(100-maxPercent) / 100
}

// purgeAndPrune applies retention (`preserve`, usage cap) plus the
// bootloader stale-entry prune on the mounted partition (PLAN.md
// §3.9: prune only when a purge deleted something, same session;
// grub + syslinux prune their own config, rpi has nothing to prune).
// It reports the deleted basenames, oldest first. A missing kernel
// dir (fresh partition) is a no-op success.
func purgeAndPrune(log Logger, partRoot, dir, flavor string, preserve, maxPercent int, protected map[string]bool) ([]string, error) {
	total, free, _, err := PathInfo(partRoot)
	if err != nil {
		return nil, err
	}
	entries, err := listKernels(partRoot, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Debug("update: no kernel dir; nothing to purge", "dir", dir)
			return nil, nil
		}
		return nil, err
	}
	target := targetFreeFromPercent(total, maxPercent)
	deleted := planPurge(ownFlavorEntries(entries, flavor), protected, preserve, free, target)
	if err := applyPurge(partRoot, dir, deleted); err != nil {
		return nil, err
	}
	if len(deleted) > 0 {
		pruneGrubFile(partRoot, dir, flavor, log)
		pruneSyslinuxFile(partRoot, dir, flavor, log)
	}
	return deleted, nil
}
