package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// runList implements `nodectl list`: staged versions + running +
// current bootloader default. Read-only; shared lock only.
func runList(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "Usage: nodectl list\n") }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitMisuse
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "Usage: nodectl list\n")
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
	versions, err := updatecore.ListPartitionVersions(mnt, store.KernelDir())
	if err != nil {
		log.Error("listing staged versions failed", "err", err)
		return exitOperational
	}
	def := updatecore.GetBootloaderDefault(updatecore.BootloaderAuto, mnt)
	bt, err := updatecore.DetectBootloader(mnt)
	if err != nil {
		log.Error("bootloader detection failed", "err", err)
		return exitOperational
	}
	fmt.Printf("running: %s\nbootloader: %s\ndefault: %s\nstaged:\n", osRelease(), bt, defDisplay(def))
	for _, v := range versions {
		fmt.Printf("  %s\n", v)
	}
	return exitOK
}
