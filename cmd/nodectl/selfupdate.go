package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// Self-update (PLAN.md §3.14, M7): `nodectl selfupdate` refreshes the
// CLI binary itself from the simplek8s-nodectl channels, and every
// other subcommand runs a daily best-effort pre-check
// (maybeAutoSelfupdate, wired in main.go).
//
// Trust and selection mirror the kernel flow: the channel's signed
// SHA256SUMS is fetched via updatecore.FetchVerifiedIndex, arch uses
// the shared channel vocabulary, and `latest` is the newest TS
// computed from the verified index. State (last attempt, UTC
// RFC3339) lives in /run/simplek8s/nodectl-selfcheck; the update
// lock is never taken (binary + state only, state under its own
// flock). A successful install hands execution to the new binary
// via syscall.Exec (NODECTL_REEXEC_FROM carries the old sha for the
// report, which is emitted by the NEW binary).

// defaultCLIRepoURL is the default nodectl release channel (a var,
// not a const, so channel-pinned builds stay possible via -ldflags
// — same pattern as version/commit/builtAt in main.go).
var defaultCLIRepoURL = "https://dl.simplek8s.org/simplek8s-nodectl/stable"

const (
	// selfStatePath records the last selfupdate attempt (explicit or
	// auto). /run is tmpfs: first command after each boot checks,
	// then at most one attempt per selfStateThrottle of uptime.
	selfStatePath = "/run/simplek8s/nodectl-selfcheck"

	// selfStateThrottle is the minimum age of the last attempt
	// before the auto path checks again. The explicit subcommand
	// always checks (M7: ignores the last-check time).
	selfStateThrottle = 24 * time.Hour

	// autoSelfTimeout bounds the background check: a stalled
	// network must never hold the real subcommand hostage. The
	// explicit subcommand keeps the 5min client.
	autoSelfTimeout = 30 * time.Second

	// reexecEnv carries the pre-update sha256 across the
	// syscall.Exec handoff so the NEW binary emits the report.
	reexecEnv = "NODECTL_REEXEC_FROM"

	// maxCLIBinaryBytes caps the selfupdate download (the
	// published upx-packed binary is single-digit MB; kernel
	// artifacts stream uncapped through staging instead).
	maxCLIBinaryBytes = 64 << 20
)

// cliRepoBase trims one trailing slash, keeping the short-channel
// expansion parallel to the kernel channels (dev | rolling |
// stable) under the simplek8s-nodectl/ prefix.
func cliRepoBase(u string) string {
	switch u {
	case "dev":
		return "https://dl.simplek8s.org/simplek8s-nodectl/dev"
	case "rolling":
		return "https://dl.simplek8s.org/simplek8s-nodectl/rolling"
	case "stable":
		return defaultCLIRepoURL
	default:
		return strings.TrimSuffix(u, "/")
	}
}

// runSelfupdate implements `nodectl selfupdate`: check the channel,
// install the newest indexed binary when it differs from the
// running one, hand off via exec. Root required. Exits 0/1/2.
func runSelfupdate(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("selfupdate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var url, keyring string
	var dryRun bool
	fs.StringVar(&url, "url", defaultCLIRepoURL, "release channel (dev|rolling|stable) or custom base URL")
	fs.StringVar(&keyring, "keyring", "", "custom keyring path (default: embedded; /dev/null skips GPG verification)")
	fs.BoolVar(&dryRun, "dry-run", false, "print what would be installed; download and touch nothing")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitMisuse
	}
	if fs.NArg() != 0 {
		subcommandUsage(fs, "Usage: nodectl selfupdate [flags]")
		return exitMisuse
	}
	// Handoff landing (M7 D9): the previous binary just installed
	// this one and exec'd it — no network, report from the NEW
	// binary, then done.
	if from := os.Getenv(reexecEnv); from != "" {
		own, err := ownBinarySHA()
		if err != nil {
			log.Error("reading own binary failed", "err", err)
			return exitOperational
		}
		fmt.Printf("updated %s -> %s\n", from, own)
		return exitOK
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	base := cliRepoBase(url)
	ctx := context.Background()

	exe, err := ownBinaryPath()
	if err != nil {
		log.Error("resolving own binary path failed", "err", err)
		return exitOperational
	}
	own, err := ownBinarySHA()
	if err != nil {
		log.Error("reading own binary failed", "err", err)
		return exitOperational
	}
	sums, code := fetchIndex(ctx, log, base, keyring)
	if code != exitOK {
		refreshSelfState(log)
		return code
	}
	arch, ok := updatecore.NodectlArch(runtime.GOARCH)
	if !ok {
		log.Error("unsupported arch for selfupdate", "goarch", runtime.GOARCH)
		return exitMisuse
	}
	ts, file, sum, ok := updatecore.FilterNodectlIndex(sums, arch)
	if !ok {
		log.Error("no indexed nodectl release for arch", "arch", arch)
		refreshSelfState(log)
		return exitOperational
	}
	if sum == own {
		fmt.Printf("already current (%s)\n", own)
		refreshSelfState(log)
		return exitOK
	}
	if dryRun {
		fmt.Printf("would update %s -> %s (%s)\n", own, ts, file)
		return exitOK
	}
	if err := downloadAndInstall(ctx, httpClient(), base, file, sum, exe); err != nil {
		log.Error("selfupdate failed", "err", err)
		refreshSelfState(log)
		return exitOperational
	}
	refreshSelfState(log)
	handoffExec(log, exe, own)
	return exitOK // unreachable after a successful exec
}

// autoCheckCommand reports whether cmd triggers the daily selfupdate
// pre-check: every subcommand but version/help/selfupdate itself
// (PLAN.md §3.14).
func autoCheckCommand(cmd string) bool {
	switch cmd {
	case "check", "update", "list", "purge", "boot":
		return true
	default:
		return false
	}
}

// maybeAutoSelfupdate runs the daily best-effort pre-check before
// the real subcommand (PLAN.md §3.14): default channel/keyring
// only, suppressed by opt-out env, non-root, or a pending dry-run.
// Never writes to stdout, never alters the caller's outcome —
// warnings on stderr only.
func maybeAutoSelfupdate(log *slog.Logger, args []string) {
	if os.Getenv("NODECTL_NO_SELFUPDATE") == "1" {
		return
	}
	if os.Geteuid() != 0 {
		return
	}
	if hasDryRunFlag(args) {
		return
	}
	last, ok := readSelfState()
	if ok && !selfStateDue(last, time.Now()) {
		return
	}
	if err := autoSelfupdateAttempt(log); err != nil {
		log.Warn("auto selfupdate check failed", "err", err)
	}
}

// autoSelfupdateAttempt is one background check+install. On success
// with new bytes it execs into the new binary (the pending
// subcommand then runs there, silently); anything else just
// refreshes the state.
func autoSelfupdateAttempt(log *slog.Logger) error {
	ctx := context.Background()
	exe, err := ownBinaryPath()
	if err != nil {
		return err
	}
	own, err := ownBinarySHA()
	if err != nil {
		return err
	}
	sums, err := updatecore.FetchVerifiedIndex(ctx, httpClientWithTimeout(autoSelfTimeout), log, defaultCLIRepoURL, "", embeddedPubring)
	if err != nil {
		refreshSelfState(log)
		return err
	}
	arch, ok := updatecore.NodectlArch(runtime.GOARCH)
	if !ok {
		return fmt.Errorf("unsupported arch %q", runtime.GOARCH)
	}
	_, file, sum, ok := updatecore.FilterNodectlIndex(sums, arch)
	if !ok {
		refreshSelfState(log)
		return fmt.Errorf("no indexed nodectl release for arch %q", arch)
	}
	if sum == own {
		refreshSelfState(log)
		return nil
	}
	if err := downloadAndInstall(ctx, httpClientWithTimeout(autoSelfTimeout), defaultCLIRepoURL, file, sum, exe); err != nil {
		refreshSelfState(log)
		return err
	}
	refreshSelfState(log)
	return syscall.Exec(exe, os.Args, append(os.Environ(), reexecEnv+"="+own))
}

// ownBinaryPath resolves the running binary's path (symlinks
// evaluated): the install target and the exec handoff source.
func ownBinaryPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// ownBinarySHA returns the hex sha256 of the running binary's bytes
// (compared against the index checksum; the distro ships the exact
// published bytes, M7 D5).
func ownBinarySHA() (string, error) {
	exe, err := ownBinaryPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// downloadAndInstall streams base/file to a temp file next to exe,
// hash-verifies it, and atomically renames it over exe. Silent by
// design (the auto path must not touch stdout; the success report
// is emitted by the new binary after the handoff).
func downloadAndInstall(ctx context.Context, client *http.Client, base, file, wantSHA, exe string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+file, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, file)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".nodectl-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	installed := false
	defer func() {
		tmp.Close()
		if !installed {
			os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxCLIBinaryBytes+1))
	if err != nil {
		return err
	}
	if n > maxCLIBinaryBytes {
		return fmt.Errorf("binary exceeds %d bytes", maxCLIBinaryBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch for %s", file)
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err = os.Rename(tmpName, exe); err != nil {
		return err
	}
	installed = true
	return nil
}

// handoffExec passes execution to the freshly installed binary
// (M7 D9). The state timestamp is already written, so the new
// process never re-triggers; an exec failure degrades to exit 0 —
// the binary is installed either way.
func handoffExec(log *slog.Logger, exe, oldSHA string) {
	if err := syscall.Exec(exe, os.Args, append(os.Environ(), reexecEnv+"="+oldSHA)); err != nil {
		log.Warn("handing off to the updated binary failed (binary is installed)", "err", err)
	}
}

// readSelfState returns the last attempt timestamp. ok=false covers
// missing, unreadable and corrupt content (all mean: check now).
func readSelfState() (ts time.Time, ok bool) {
	raw, err := os.ReadFile(selfStatePath)
	if err != nil {
		return time.Time{}, false
	}
	ts, err = time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// selfStateDue reports whether the auto path must check now: no
// usable record, or the last attempt is at least a throttle old.
func selfStateDue(last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= selfStateThrottle
}

// refreshSelfState records an attempt (explicit runs included, so a
// later auto-check does not repeat it). Best-effort: the state file
// carries its own flock (never the update lock), and any failure is
// the caller's to report or ignore.
func refreshSelfState(log *slog.Logger) {
	unlock, err := updatecore.LockFile(selfStatePath, true)
	if err != nil {
		log.Warn("selfupdate state lock failed", "err", err)
		return
	}
	defer unlock()
	if err := os.WriteFile(selfStatePath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		log.Warn("selfupdate state write failed", "err", err)
	}
}

// hasDryRunFlag scans raw argv for the dry-run flag (either dash
// form, with or without =value). Positional args never match: the
// token must start with '-'.
func hasDryRunFlag(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name == "dry-run" {
			return true
		}
	}
	return false
}
