package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/simplek8s/simplek8s-controller/internal/features/update"
)

// runPurge implements `simplek8sctl purge`: retention + bootloader
// prune in one mounted session. It never prompts (PLAN-M6 D18);
// `--dry-run` previews. Exclusive lock.
func runPurge(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var preserve, maxUsage int
	var dryRun bool
	fs.IntVar(&preserve, "preserve", 3, "staged versions to keep")
	fs.IntVar(&maxUsage, "max-percent-usage", 75, "boot partition usage cap percent")
	fs.BoolVar(&dryRun, "dry-run", false, "print what would be deleted; touch nothing")
	if err := fs.Parse(args); err != nil {
		return exitMisuse
	}
	if fs.NArg() != 0 {
		subcommandUsage(fs, "Usage: simplek8sctl purge [flags]")
		return exitMisuse
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
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
	kernels, err := update.ListPartitionKernels(mnt, dir)
	if err != nil {
		log.Error("listing staged kernels failed", "err", err)
		return exitOperational
	}
	flavor, ok := detectFlavor(kernels)
	if !ok {
		log.Error("board flavor unresolvable (no staged flavor, no device-tree)")
		return exitMisuse
	}
	running := osRelease()
	if dryRun {
		deleted, err := update.PreviewPurge(log, mnt, dir, flavor, preserve, maxUsage, running, update.BootloaderAuto)
		if err != nil {
			log.Error("purge preview failed", "err", err)
			return exitOperational
		}
		fmt.Printf("would delete: %v\n", deleted)
		return exitOK
	}
	deleted, err := update.PurgePartition(log, mnt, dir, flavor, preserve, maxUsage, running, update.BootloaderAuto)
	if err != nil {
		log.Error("purge failed", "err", err)
		return exitOperational
	}
	after, err := update.ListPartitionVersions(mnt, dir)
	if err != nil {
		log.Error("listing staged versions failed", "err", err)
		return exitOperational
	}
	fmt.Printf("deleted: %v\nkept: %v\n", deleted, after)
	return exitOK
}
