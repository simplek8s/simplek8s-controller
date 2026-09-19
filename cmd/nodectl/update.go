package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// runUpdate implements `nodectl update [<ts>]`: check + download +
// verify + extract + stage + retention + bootloader re-point (iff
// --next-kernel). No arg stages newest. Exclusive lock.
func runUpdate(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var url, keyring string
	var nextKernel bool
	var preserve, maxUsage int
	var dryRun bool
	fs.StringVar(&url, "url", defaultRepoURL, "release channel (dev|rolling|stable) or custom base URL")
	fs.StringVar(&keyring, "keyring", "", "custom keyring path (default: embedded; /dev/null skips GPG verification)")
	fs.BoolVar(&nextKernel, "next-kernel", true, "re-point the bootloader default at the staged version")
	fs.IntVar(&preserve, "preserve", 3, "staged versions to keep")
	fs.IntVar(&maxUsage, "max-percent-usage", 75, "boot partition usage cap percent")
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan; touch nothing on the partition")
	if err := fs.Parse(args); err != nil {
		return exitMisuse
	}
	if fs.NArg() > 1 {
		subcommandUsage(fs, "Usage: nodectl update [flags] [<ts>] (flags before the ts)")
		return exitMisuse
	}
	var wantTS string
	if fs.NArg() == 1 {
		wantTS = fs.Arg(0)
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	base := repoBase(url)
	ctx := context.Background()
	store := openStore(log)

	unlock, err := store.LockExcl()
	if err != nil {
		log.Error("boot partition session busy", "err", err)
		return exitOperational
	}
	defer unlock()
	mnt, cleanup, err := store.MountedBoot(ctx)
	if err != nil {
		log.Error("boot device discovery failed", "err", err)
		return exitOperational
	}
	defer cleanup()
	dir := store.KernelDir()

	kernels, err := updatecore.ListPartitionKernels(mnt, dir)
	if err != nil {
		log.Error("listing staged kernels failed", "err", err)
		return exitOperational
	}
	before, err := updatecore.ListPartitionVersions(mnt, dir)
	if err != nil {
		log.Error("listing staged versions failed", "err", err)
		return exitOperational
	}
	flavor, ok := detectFlavor(kernels)
	if !ok {
		log.Error("board flavor unresolvable (no staged flavor, no device-tree)")
		return exitMisuse
	}
	running := osRelease()

	sums, code := fetchIndex(ctx, log, base, keyring)
	if code != exitOK {
		return code
	}
	var rel updatecore.Release
	if wantTS != "" {
		var found bool
		rel, found = updatecore.LookupRelease(sums, wantTS, flavor)
		if !found {
			log.Error("ts not in verified index for flavor", "ts", wantTS, "flavor", flavor)
			return exitOperational
		}
	} else {
		var found bool
		rel, found = updatecore.FilterIndexByFlavor(sums, flavor)
		if !found {
			log.Error("no indexed release for flavor", "flavor", flavor)
			return exitOperational
		}
	}

	// Hash gate (PLAN-M6 auto-overwrite): staged bytes matching the
	// verified .efi hash skip the download entirely.
	stored := updatecore.StoredKernelName(rel.TS, flavor)
	stagedPath := filepath.Join(mnt, dir, stored)
	if st, serr := os.Stat(stagedPath); serr == nil && !st.IsDir() {
		if rel.EfiHash != "" {
			raw, rerr := os.ReadFile(stagedPath)
			if rerr != nil {
				log.Error("reading staged kernel failed", "err", rerr)
				return exitOperational
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) == rel.EfiHash {
				fmt.Printf("staged %s (already current, no download)\n", rel.TS)
				return ensureBootGoal(log, mnt, dir, flavor, rel.TS, nextKernel)
			}
			log.Warn("staged file differs from verified hash; replacing", "ts", rel.TS)
		} else {
			fmt.Printf("staged %s (already staged)\n", rel.TS)
			return ensureBootGoal(log, mnt, dir, flavor, rel.TS, nextKernel)
		}
	}

	// Capacity pre-check before any download (D16): statfs must work
	// and the partition must not be completely full; exact fit is
	// enforced with purge-to-fit inside staging.
	if _, free, _, err := updatecore.PathInfo(mnt); err != nil {
		log.Error("boot partition stat failed", "err", err)
		return exitOperational
	} else if free == 0 {
		log.Error("boot partition full")
		return exitOperational
	}

	work, err := os.MkdirTemp("", "nodectl-stage-")
	if err != nil {
		log.Error("scratch dir failed", "err", err)
		return exitOperational
	}
	defer os.RemoveAll(work)

	req := updatecore.StageRequest{
		Version:         rel.TS,
		Arch:            flavor,
		Checksum:        rel.Checksum,
		ArtifactFile:    rel.Artifact,
		RepoBase:        base,
		Preserve:        preserve,
		MaxPercentUsage: maxUsage,
		Running:         running,
		Bootloader:      updatecore.BootloaderAuto,
		EfiChecksum:     rel.EfiHash,
		NoRepoint:       !nextKernel,
		DryRun:          dryRun,
	}
	if err := updatecore.StagePartition(ctx, httpClient(), log, req, mnt, dir, work); err != nil {
		log.Error("staging failed", "err", err)
		return exitOperational
	}
	if dryRun {
		fmt.Printf("dry-run %s (no writes)\n", rel.TS)
		return exitOK
	}
	after, err := updatecore.ListPartitionVersions(mnt, dir)
	if err != nil {
		log.Error("listing staged versions failed", "err", err)
		return exitOperational
	}
	deleted, _ := versionsBeforeAfter(before, after)
	def := updatecore.GetBootloaderDefault(updatecore.BootloaderAuto, mnt)
	fmt.Printf("staged %s\ndefault: %s\npurged: %v\n", rel.TS, def, deleted)
	return exitOK
}

// ensureBootGoal applies the boot-goal half on the already-mounted
// session: with nextKernel the default must point at ts (re-point
// when needed), else it is left untouched.
func ensureBootGoal(log *slog.Logger, mnt, dir, flavor, ts string, nextKernel bool) int {
	want := "/" + filepath.Join(dir, updatecore.StoredKernelName(ts, flavor))
	if !nextKernel {
		log.Info("leaving bootloader default untouched (--next-kernel=false)")
		return exitOK
	}
	cur := updatecore.GetBootloaderDefault(updatecore.BootloaderAuto, mnt)
	if cur == want {
		fmt.Printf("default: %s (unchanged)\n", cur)
		return exitOK
	}
	if err := updatecore.SetBootloaderDefault(updatecore.BootloaderAuto, mnt, want, "/"); err != nil {
		log.Error("re-pointing bootloader default failed", "err", err)
		return exitOperational
	}
	fmt.Printf("default: %s -> %s\n", cur, want)
	return exitOK
}
