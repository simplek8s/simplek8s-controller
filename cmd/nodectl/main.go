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
	// releaseTS is the channel release TS stamped at build time
	// (the same TS as the binary filename / publish, PLAN.md §3.14
	// D14). Empty for unstamped builds: selfupdate then falls back
	// to the checksum-only newness rule.
	releaseTS = ""
)

// Exit codes (PLAN-M6 D9, plus the M9 idle exit).
const (
	exitOK          = 0
	exitOperational = 1
	exitMisuse      = 2
	// exitIdle means "nothing to manage, not an error"
	// (PLAN.md §3.16, M9 D6: `nodectl wizard` on a worker
	// node refuses to serve).
	exitIdle = 3
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitMisuse)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	if autoCheckCommand(cmd) {
		maybeAutoSelfupdate(log, args)
	}
	switch cmd {
	case "version", "-version", "--version":
		line := fmt.Sprintf("nodectl %s (%s, built %s", version, commit, builtAt)
		if releaseTS != "" {
			line += ", release " + releaseTS
		}
		fmt.Printf("%s)\n", line)
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
	case "install":
		code = runInstall(log, args)
	case "wizard":
		code = runWizard(log, args)
	case "selfupdate":
		code = runSelfupdate(log, args)
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
	fmt.Fprintf(os.Stderr, `nodectl %s (%s) — local-node admin

Usage: nodectl <command> [flags]

Commands:
  check            newest indexed release vs running vs staged
  update [<ts>]    stage a release + retention + re-point
  list             staged versions + running + bootloader default (read-only)
  purge            retention + bootloader prune (never prompts)
  boot [<ts>]      show the bootloader default, or re-point it
  install <device> install the distro IMG onto a whole-disk device
  wizard           web setup wizard (HTTPS, exit 3 when idle)
  selfupdate       check the nodectl channel + install latest
  version          print embedded build info (no root needed)

Exits: 0 ok, 1 operational error, 2 misuse, 3 idle (wizard only).
Root required (except version).
`, version, commit)
}

// subcommandUsage prints a command's flag defaults to stderr.
func subcommandUsage(fs *flag.FlagSet, header string) {
	fmt.Fprintf(os.Stderr, "%s\n\nFlags:\n", header)
	fs.PrintDefaults()
}
