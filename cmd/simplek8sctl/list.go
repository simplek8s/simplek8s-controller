package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/simplek8s/simplek8s-controller/internal/features/update"
)

// runList implements `simplek8sctl list`: staged versions + running +
// current bootloader default. Read-only; shared lock only.
func runList(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitMisuse
	}
	if fs.NArg() != 0 {
		subcommandUsage(fs, "Usage: simplek8sctl list")
		return exitMisuse
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	ctx := context.Background()
	store := openStore(log)
	unlock, err := store.LockShared()
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
	versions, err := update.ListPartitionVersions(mnt, store.KernelDir())
	if err != nil {
		log.Error("listing staged versions failed", "err", err)
		return exitOperational
	}
	def := update.GetBootloaderDefault(update.BootloaderAuto, mnt)
	bt, err := update.DetectBootloader(mnt)
	if err != nil {
		log.Error("bootloader detection failed", "err", err)
		return exitOperational
	}
	fmt.Printf("running: %s\nbootloader: %s\ndefault: %s\nstaged:\n", osRelease(), bt, def)
	for _, v := range versions {
		fmt.Printf("  %s\n", v)
	}
	return exitOK
}
