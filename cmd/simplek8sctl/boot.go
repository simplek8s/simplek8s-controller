package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// runBoot implements `simplek8sctl boot show|set <ts>`: inspect or
// re-point the bootloader default without downloading. `set` refuses
// a ts whose file is absent (file-first, PLAN.md §3.10).
func runBoot(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "print what set would do; touch nothing")
	if err := fs.Parse(args); err != nil {
		return exitMisuse
	}
	rest := fs.Args()
	if len(rest) == 0 {
		subcommandUsage(fs, "Usage: simplek8sctl boot [--dry-run] show|set <ts>")
		return exitMisuse
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			subcommandUsage(fs, "Usage: simplek8sctl boot show")
			return exitMisuse
		}
		return runBootShow(log)
	case "set":
		if len(rest) != 2 || rest[1] == "" {
			subcommandUsage(fs, "Usage: simplek8sctl boot [--dry-run] set <ts>")
			return exitMisuse
		}
		return runBootSet(log, rest[1], dryRun)
	default:
		subcommandUsage(fs, "Usage: simplek8sctl boot [--dry-run] show|set <ts>")
		return exitMisuse
	}
}

func runBootShow(log *slog.Logger) int {
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
	bt, err := updatecore.DetectBootloader(mnt)
	if err != nil {
		log.Error("bootloader detection failed", "err", err)
		return exitOperational
	}
	def := updatecore.GetBootloaderDefault(updatecore.BootloaderAuto, mnt)
	kernels, err := updatecore.ListPartitionKernels(mnt, store.KernelDir())
	if err != nil {
		log.Error("listing staged kernels failed", "err", err)
		return exitOperational
	}
	fmt.Printf("bootloader: %s\ndefault: %s\nstaged:\n", bt, def)
	for _, k := range kernels {
		fmt.Printf("  %s\n", k)
	}
	return exitOK
}

func runBootSet(log *slog.Logger, ts string, dryRun bool) int {
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
	kernels, err := updatecore.ListPartitionKernels(mnt, store.KernelDir())
	if err != nil {
		log.Error("listing staged kernels failed", "err", err)
		return exitOperational
	}
	flavor, ok := detectFlavor(kernels)
	if !ok {
		log.Error("board flavor unresolvable (no staged flavor, no device-tree)")
		return exitMisuse
	}
	want := "/" + filepath.Join(store.KernelDir(), updatecore.StoredKernelName(ts, flavor))
	if st, serr := os.Stat(filepath.Join(mnt, want)); serr != nil || st.IsDir() {
		log.Error("refusing boot set: kernel file absent from boot partition", "ts", ts)
		return exitOperational
	}
	cur := updatecore.GetBootloaderDefault(updatecore.BootloaderAuto, mnt)
	if dryRun {
		fmt.Printf("would set default: %s -> %s\n", cur, want)
		return exitOK
	}
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
