// Command nodectl manages the local node only (PLAN-M6): no
// controller, no cluster access, no node annotations. Flat
// subcommands, stdlib flag + log/slog, exits 0 ok / 1 operational
// error / 2 misuse. Must run as root on the node itself.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// Build information, injected at compile time via -ldflags (see
// Makefile build-nodectl). Defaults for plain `go run`/`go build`.
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

// Exit codes (PLAN-M6 D9).
const (
	exitOK          = 0
	exitOperational = 1
	exitMisuse      = 2
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitMisuse)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "version", "-version", "--version":
		fmt.Printf("nodectl %s (%s, built %s)\n", version, commit, builtAt)
		return
	case "check":
		code = runCheck(log, args)
	case "update":
		code = runUpdate(log, args)
	case "list":
		code = runList(log, args)
	case "purge":
		code = runPurge(log, args)
	case "boot":
		code = runBoot(log, args)
	case "-h", "-help", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		code = exitMisuse
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintf(os.Stderr, `nodectl %s (%s) — local-node admin (see PLAN.md §3.13)

Usage: nodectl <command> [flags]

Commands (flat; install reserved):
  check            newest indexed release vs running vs staged (read-only)
  update [<ts>]     stage a release + retention + bootloader re-point
  list             staged versions + running + bootloader default (read-only)
  purge            retention + bootloader prune (never prompts)
  boot show        bootloader type + default + staged entries (read-only)
  boot set <ts>    re-point the bootloader default (file must exist)
  version          print embedded build info (no root needed)

Exits: 0 ok, 1 operational error, 2 misuse. Root required (except version).
`, version, commit)
}

// subcommandUsage prints a command's flag defaults to stderr.
func subcommandUsage(fs *flag.FlagSet, header string) {
	fmt.Fprintf(os.Stderr, "%s\n\nFlags:\n", header)
	fs.PrintDefaults()
}
