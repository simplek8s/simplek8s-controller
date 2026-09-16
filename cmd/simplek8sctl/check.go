package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/simplek8s/simplek8s-controller/internal/features/update"
)

// runCheck implements `simplek8sctl check`: fetch + verify the index,
// report newest indexed ts for the node's flavor vs running vs staged.
// Read-only (no mount writes); shared lock only.
func runCheck(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var url, keyring string
	var verbose bool
	fs.StringVar(&url, "url", defaultRepoURL, "release channel (dev|rolling|stable) or custom base URL")
	fs.StringVar(&keyring, "keyring", "", "custom keyring path (default: embedded; /dev/null skips GPG verification)")
	fs.BoolVar(&verbose, "verbose", false, "list all remote ts of the flavor + all staged + keyring/URL")
	if err := fs.Parse(args); err != nil {
		return exitMisuse
	}
	if fs.NArg() != 0 {
		subcommandUsage(fs, "Usage: simplek8sctl check [--url URL] [--keyring PATH] [--verbose]")
		return exitMisuse
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	base := repoBase(url)
	ctx := context.Background()
	store := openStore(log)

	unlock, err := store.LockShared()
	if err != nil {
		log.Error("boot partition session busy", "err", err)
		return exitOperational
	}
	mnt, cleanup, err := store.MountedBoot(ctx)
	if err != nil {
		unlock()
		log.Error("boot device discovery failed", "err", err)
		return exitOperational
	}
	kernels, err := update.ListPartitionKernels(mnt, store.KernelDir())
	cleanup()
	unlock()
	if err != nil {
		log.Error("listing staged kernels failed", "err", err)
		return exitOperational
	}
	flavor, ok := detectFlavor(kernels)
	if !ok {
		log.Error("board flavor unresolvable (no staged flavor, no device-tree)")
		return exitMisuse
	}
	stagedNewest := ""
	for _, k := range kernels {
		if ts, _, ok := update.ParseStoredKernel(k); ok {
			if stagedNewest == "" || update.NewerTS(ts, stagedNewest) {
				stagedNewest = ts
			}
		}
	}
	sums, code := fetchIndex(ctx, log, base, keyring)
	if code != exitOK {
		return code
	}
	rel, ok := update.FilterIndexByFlavor(sums, flavor)
	if !ok {
		log.Error("no indexed release for flavor", "flavor", flavor)
		return exitOperational
	}
	running := osRelease()
	verdict := "up-to-date"
	if running == "" || update.NewerTS(rel.TS, running) {
		verdict = "update available " + rel.TS
	}
	fmt.Printf("flavor: %s\nrunning: %s\nstaged-newest: %s\nremote-newest: %s\nverdict: %s\n",
		flavor, running, stagedNewest, rel.TS, verdict)
	if verbose {
		fmt.Printf("url: %s\nartifact: %s\n", base, rel.Artifact)
		fmt.Printf("remote-ts:\n")
		seen := map[string]bool{}
		for file := range sums {
			if ts, fa, ok := update.ParseKernelRelease(file); ok && fa == flavor && !seen[ts] {
				seen[ts] = true
				fmt.Printf("  %s\n", ts)
			}
		}
		fmt.Printf("staged:\n")
		for _, k := range kernels {
			fmt.Printf("  %s\n", k)
		}
	}
	return exitOK
}
