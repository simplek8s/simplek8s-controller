package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// Distro install onto a whole disk (PLAN.md §3.15, M8): download the
// verified `.img.zst`, stream it onto the target, append + format
// the `/var` partition, and write `simplek8s.yaml` (prompted root
// password or `--config`). Same human-output + exits 0/1/2
// discipline as §3.13. No update lock: the target is always a
// *different* disk (the running boot disk is refused via the mount
// check), so there is no shared resource with controller/CLI
// sessions (M8 D7).

const (
	// installSizeFloor is the minimum target size: 512M IMG plus
	// room for /var (M8 D10, PLAN §7.8 I9).
	installSizeFloor = 1 << 30

	// installCountdown is the cancellable pre-write delay (M8 D6).
	installCountdown = 10 * time.Second

	// installCryptSaltLen is the SHA-512-crypt salt length (spec
	// charset ./0-9A-Za-z, max 16).
	installCryptSaltLen = 16
)

// runInstall implements `nodectl install [--url] [--yes]
// [--config FILE] [--dry-run] [<ts>] <device>`.
func runInstall(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var url, config string
	var assumeYes, dryRun bool
	fs.StringVar(&url, "url", defaultRepoURL, "release channel (dev|rolling|stable) or custom base URL")
	fs.BoolVar(&assumeYes, "yes", false, "skip the confirmation countdown (required without a terminal)")
	fs.StringVar(&config, "config", "", "install FILE as simplek8s.yaml (default: prompt root password, mounts-only yaml)")
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan; download and touch nothing")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitMisuse
	}
	var wantTS, device string
	switch fs.NArg() {
	case 1:
		device = fs.Arg(0)
	case 2:
		wantTS, device = fs.Arg(0), fs.Arg(1)
	default:
		subcommandUsage(fs, "Usage: nodectl install [flags] [<ts>] <device> (flags before the ts)")
		return exitMisuse
	}
	if config != "" {
		if st, err := os.Stat(config); err != nil || st.IsDir() {
			log.Error("--config file not found", "file", config)
			return exitMisuse
		}
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	target, base, code := validateInstallTarget(log, device)
	if code != exitOK {
		return code
	}
	if sz, err := installTargetSize(base); err != nil {
		log.Error("reading target size failed", "err", err)
		return exitOperational
	} else if sz < installSizeFloor {
		log.Error("target too small (need at least 1GiB)", "device", target)
		return exitMisuse
	}

	baseURL := repoBase(url)
	ctx := context.Background()
	sums, code := fetchIndex(ctx, log, baseURL, "")
	if code != exitOK {
		return code
	}
	model, compatible := deviceTree()
	flavor, ok := updatecore.InstallFlavor(model, compatible, runtime.GOARCH)
	if !ok {
		log.Error("install flavor unresolvable for arch", "goarch", runtime.GOARCH)
		return exitMisuse
	}
	var ts, file, sum string
	if wantTS != "" {
		file = fmt.Sprintf("simplek8s.%s.%s.img.zst", wantTS, flavor)
		var found bool
		sum, found = sums[file]
		if !found || sum == "" {
			log.Error("ts not in verified index for flavor", "ts", wantTS, "flavor", flavor)
			return exitOperational
		}
		ts = wantTS
	} else {
		var found bool
		ts, file, sum, found = updatecore.FilterImgIndex(sums, flavor)
		if !found {
			log.Error("no indexed install image for flavor", "flavor", flavor)
			return exitOperational
		}
	}

	// Root password (M8 D5): --config is authoritative and never
	// prompts; otherwise prompt on a tty before anything
	// destructive. --dry-run never prompts.
	var passwordHash string
	if config == "" && !dryRun {
		if !isTerminalStdin() {
			log.Error("cannot prompt for the root password without a terminal (use --config)")
			return exitMisuse
		}
		hash, code := promptRootPassword(log)
		if code != exitOK {
			return code
		}
		passwordHash = hash
	}

	if dryRun {
		fmt.Printf("device: %s\nimg: %s (%s)\nlayout: p1 ESP (from IMG), p2 var ext4 (rest of disk)\n", target, ts, file)
		if config != "" {
			fmt.Printf("config: %s\n", config)
		} else {
			fmt.Printf("config: minimal (mounts /var, would prompt root password)\n")
		}
		return exitOK
	}

	if code := confirmInstallDestroy(target, assumeYes); code != exitOK {
		return code
	}
	if err := streamImageToDisk(ctx, httpClient(), baseURL, file, sum, target); err != nil {
		log.Error("install failed", "err", err)
		return exitOperational
	}
	p1 := partDevName(target, 1)
	if code := appendVarPartition(log, target); code != exitOK {
		return code
	}
	if code := writeInstallYAML(log, p1, config, ts, passwordHash); code != exitOK {
		return code
	}
	printInstallSummary(target, ts, config)
	return exitOK
}

// validateInstallTarget normalizes the device path (resolving
// /dev/disk/by-* symlinks) and enforces whole-disk,
// not-a-partition, nothing-mounted. Returns the canonical target +
// its sysfs base name.
func validateInstallTarget(log *slog.Logger, device string) (target, base string, code int) {
	target = device
	if !strings.Contains(target, "/") {
		target = "/dev/" + target
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if st, err := os.Stat(target); err != nil || !isBlockDevice(st) {
		log.Error("not a block device", "device", target)
		return "", "", exitMisuse
	}
	base = filepath.Base(target)
	if st, err := os.Stat("/sys/block/" + base); err != nil || !st.IsDir() {
		matches, _ := filepath.Glob("/sys/block/*/" + base)
		if len(matches) > 0 {
			holder := filepath.Base(filepath.Dir(matches[0]))
			log.Error("is a partition; use the whole disk", "device", target, "disk", "/dev/"+holder)
		} else {
			log.Error("not a known disk device", "device", target)
		}
		return "", "", exitMisuse
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		raw, err = os.ReadFile("/proc/mounts")
		if err != nil {
			log.Error("reading mounts failed", "err", err)
			return "", "", exitOperational
		}
	}
	if installTargetMounted(string(raw), target, base) {
		log.Error("target has mounted filesystems, unmount first", "device", target)
		return "", "", exitOperational
	}
	return target, base, exitOK
}

// isBlockDevice reports whether fi is a block device node
// (Linux-only; nodectl builds GOOS=linux only).
func isBlockDevice(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFBLK
}

// installTargetMounted reports whether the target or any of its
// partitions backs an active mount. Ground truth is device
// numbers, not mount source strings: live systems mount via
// aliases (/dev/disk/by-*, /dev/root), and label symlinks keep
// pointing at unmounted claimants after an install — both lie,
// major:minor never does. /proc/self/mountinfo field 3 carries it;
// plain /proc/mounts (degraded fallback) only matches literal
// sources.
func installTargetMounted(mounts, target, base string) bool {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) > 2 && strings.Contains(fields[2], ":") {
			if maj, min, ok := splitDevNo(fields[2]); ok && targetRdevSet(target, base)[[2]int{maj, min}] {
				return true
			}
			continue
		}
		if mountSourceMatchesTarget(fields[0], target) {
			return true
		}
	}
	return false
}

// targetRdevSet returns the (major, minor) set of the target disk
// and every partition node under /sys/block/<base>.
func targetRdevSet(target, base string) map[[2]int]bool {
	want := map[[2]int]bool{}
	if d := devNoOf(target); d != [2]int{-1, -1} {
		want[d] = true
	}
	if parts, _ := filepath.Glob("/sys/block/" + base + "/" + base + "*"); parts != nil {
		for _, p := range parts {
			if d := devNoOf("/dev/" + filepath.Base(p)); d != [2]int{-1, -1} {
				want[d] = true
			}
		}
	}
	return want
}

// mountinfoHitsTarget matches mountinfo major:minor devices against
// a precomputed set (pure: the live SimpleK8s regression test
// lives here — by-label source strings with foreign numbers stay
// silent, matching numbers fire whatever the source string says).
func mountinfoHitsTarget(mountinfo string, want map[[2]int]bool) bool {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) <= 2 || !strings.Contains(fields[2], ":") {
			continue
		}
		if maj, min, ok := splitDevNo(fields[2]); ok && want[[2]int{maj, min}] {
			return true
		}
	}
	return false
}

// splitDevNo parses "major:minor".
func splitDevNo(dev string) (maj, min int, ok bool) {
	parts := strings.SplitN(dev, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// devNoOf returns the (major, minor) of a device node ([-1,-1] when
// unstatted). Linux dev_t layout (both build arches LE): major =
// bits 8-19 + 32+, minor = bits 0-7 + 12+.
func devNoOf(path string) [2]int {
	st, err := os.Stat(path)
	if err != nil {
		return [2]int{-1, -1}
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int{-1, -1}
	}
	dev := uint64(s.Rdev)
	maj := int((dev>>8)&0xfff | (dev>>32)&0xfffff000)
	min := int((dev & 0xff) | (dev>>12)&0xffffff00)
	return [2]int{maj, min}
}

// mountSourceMatchesTarget reports whether one mount source is the
// target or one of its partitions. Only meaningful for literal
// /dev/<disk>[p]<n> sources (the /proc/mounts degraded fallback);
// the mountinfo path never consults strings.
func mountSourceMatchesTarget(src, target string) bool {
	if src == target {
		return true
	}
	rest, ok := strings.CutPrefix(src, target)
	if !ok || rest == "" {
		return false
	}
	rest = strings.TrimPrefix(rest, "p")
	return rest != "" && isDigits(rest)
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// installTargetSize reads the whole-disk size in bytes via sysfs
// (sectors × 512).
func installTargetSize(base string) (int64, error) {
	raw, err := os.ReadFile("/sys/block/" + base + "/size")
	if err != nil {
		return 0, err
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, err
	}
	return sectors * 512, nil
}

// promptRootPassword reads + confirms the root password (hidden)
// and returns its SHA-512-crypt hash. Empty is refused (exit 2):
// key-only setups belong in an explicit --config file.
func promptRootPassword(log *slog.Logger) (string, int) {
	first, err := readPasswordLine("root password: ")
	if err != nil {
		log.Error("reading password failed", "err", err)
		return "", exitOperational
	}
	second, err := readPasswordLine("confirm root password: ")
	if err != nil {
		log.Error("reading password failed", "err", err)
		return "", exitOperational
	}
	if first == "" {
		log.Error("empty root password refused (use --config for key-only setups)")
		return "", exitMisuse
	}
	if first != second {
		log.Error("passwords do not match")
		return "", exitMisuse
	}
	salt, err := cryptSalt()
	if err != nil {
		log.Error("generating salt failed", "err", err)
		return "", exitOperational
	}
	hash, err := updatecore.CryptSHA512(first, salt)
	if err != nil {
		log.Error("hashing password failed", "err", err)
		return "", exitOperational
	}
	return hash, exitOK
}

// cryptSalt returns 16 random chars from the crypt alphabet.
func cryptSalt() (string, error) {
	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var raw [installCryptSaltLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	out := make([]byte, installCryptSaltLen)
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// confirmInstallDestroy runs the cancellable pre-write countdown:
// any Enter aborts untouched (exit 1); non-tty requires --yes.
func confirmInstallDestroy(target string, assumeYes bool) int {
	if assumeYes {
		return exitOK
	}
	if !isTerminalStdin() {
		fmt.Fprintf(os.Stderr, "stdin is not a terminal; re-run with --yes to install non-interactively\n")
		return exitMisuse
	}
	fmt.Printf("\nALL DATA ON %s WILL BE DESTROYED.\nPress Ctrl+C or Enter to cancel, or wait %ds to continue.\n", target, int(installCountdown/time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(os.Stdin)
		_, _ = r.ReadString('\n')
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	remaining := int(installCountdown / time.Second)
	for remaining > 0 {
		fmt.Printf("\rContinuing in %ds... ", remaining)
		select {
		case <-done:
			fmt.Printf("\ncancelled by user, no changes made\n")
			return exitOperational
		case <-tick.C:
			remaining--
		}
	}
	fmt.Printf("\n")
	return exitOK
}

// countingReader counts streamed (compressed) bytes.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// countingWriter counts written (decompressed) bytes.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// streamImageToDisk downloads base/file, verifies the compressed
// sha256 inline, zstd-decodes in-process, and writes straight onto
// the whole-disk device — one pipeline, both counters on stderr
// (M8 D11). A hash mismatch fails with the target left dirty
// (documented re-run from scratch, M8 D8).
func streamImageToDisk(ctx context.Context, client *http.Client, base, file, wantSHA, target string) error {
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
	dev, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer dev.Close()
	var down, written atomic.Int64
	hash := sha256.New()
	dec, err := zstd.NewReader(io.TeeReader(countingReader{resp.Body, &down}, hash))
	if err != nil {
		return err
	}
	defer dec.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				fmt.Fprintf(os.Stderr, "\rdownloaded %d MiB / wrote %d MiB... ", down.Load()>>20, written.Load()>>20)
			}
		}
	}()
	if _, err := io.Copy(countingWriter{dev, &written}, dec); err != nil {
		fmt.Fprintf(os.Stderr, "\n")
		return err
	}
	fmt.Fprintf(os.Stderr, "\rdownloaded %d MiB / wrote %d MiB.\n", down.Load()>>20, written.Load()>>20)
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch for %s (target left dirty — re-run install)", file)
	}
	return dev.Sync()
}

// partDevName appends the partition number (sdX->sdX1,
// nvme0n1/mmcblk0->Xp1).
func partDevName(disk string, num int) string {
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, num)
	}
	return fmt.Sprintf("%s%d", disk, num)
}

// appendVarPartition adds p2 (type 83, rest of disk) via sfdisk,
// re-reads the table, waits for the node, and formats it ext4
// (label var).
func appendVarPartition(log *slog.Logger, target string) int {
	cmd := exec.Command("sfdisk", "--force", "--append", target)
	cmd.Stdin = strings.NewReader(", ,83\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Error("sfdisk append failed", "err", err, "out", strings.TrimSpace(string(out)))
		return exitOperational
	}
	if out, err := exec.Command("blockdev", "--rereadpt", target).CombinedOutput(); err != nil {
		log.Error("partition re-read failed", "err", err, "out", strings.TrimSpace(string(out)))
		return exitOperational
	}
	if _, err := exec.LookPath("udevadm"); err == nil {
		_ = exec.Command("udevadm", "settle", "--timeout=10").Run()
	}
	p2 := partDevName(target, 2)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, serr := os.Stat(p2); serr == nil && isBlockDevice(st) {
			break
		}
		if time.Now().After(deadline) {
			log.Error("partition node did not appear", "node", p2)
			return exitOperational
		}
		time.Sleep(time.Second)
	}
	if out, err := exec.Command("mkfs.ext4", "-L", "var", "-F", "-q", p2).CombinedOutput(); err != nil {
		log.Error("mkfs.ext4 failed", "err", err, "out", strings.TrimSpace(string(out)))
		return exitOperational
	}
	return exitOK
}

// writeInstallYAML mounts the ESP and writes
// simplek8s/simplek8s.yaml (minimal or --config verbatim).
func writeInstallYAML(log *slog.Logger, esp, config, ts, passwordHash string) int {
	mnt, err := os.MkdirTemp("", "nodectl-install-")
	if err != nil {
		log.Error("scratch dir failed", "err", err)
		return exitOperational
	}
	defer os.RemoveAll(mnt)
	if out, err := exec.Command("mount", esp, mnt).CombinedOutput(); err != nil {
		log.Error("mounting ESP failed", "err", err, "out", strings.TrimSpace(string(out)))
		return exitOperational
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			log.Warn("unmounting ESP failed", "err", err, "out", strings.TrimSpace(string(out)))
		}
	}()
	var content []byte
	if config != "" {
		content, err = os.ReadFile(config)
		if err != nil {
			log.Error("reading --config failed", "err", err)
			return exitOperational
		}
	} else {
		content = []byte(minimalInstallYAML(ts, passwordHash))
	}
	dir := filepath.Join(mnt, "simplek8s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Error("creating simplek8s dir failed", "err", err)
		return exitOperational
	}
	if err := os.WriteFile(filepath.Join(dir, "simplek8s.yaml"), content, 0o644); err != nil {
		log.Error("writing simplek8s.yaml failed", "err", err)
		return exitOperational
	}
	syscall.Sync()
	return exitOK
}

// minimalInstallYAML renders the default node config: root login
// plus the /var mount (init defaults Version to "1").
func minimalInstallYAML(ts, passwordHash string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Written by nodectl install (%s). Provision network/keys on\n", ts)
	sb.WriteString("# first boot via the wizard (port 5443) or replace this file\n# with your own (see simplek8s.yaml.example).\nusers:\n  - name: root\n")
	fmt.Fprintf(&sb, "    password_hash: %s\n", passwordHash)
	sb.WriteString("storage:\n  mounts:\n    - what: /dev/disk/by-label/var\n      where: /var\n")
	return sb.String()
}

// printInstallSummary reports the result (bootloader chain comes
// with the IMG; nothing to re-point on day one).
func printInstallSummary(target, ts, config string) {
	fmt.Printf("\nInstalled SimpleK8s %s on %s\n", ts, target)
	if out, err := exec.Command("lsblk", "-o", "NAME,SIZE,TYPE,LABEL", target).Output(); err == nil {
		fmt.Printf("%s", out)
	}
	if config != "" {
		fmt.Printf("config: installed from %s\n", config)
	} else {
		fmt.Printf("config: minimal (root login + mounts /var); provision network/keys via the wizard (port 5443)\n")
	}
	warnForeignLabels(target)
}

// warnForeignLabels shouts when another visible disk carries the
// LABEL=var or LABEL=EFI the install just wrote (M8 D12): the
// distro mounts /var by label at boot, and boot-device discovery
// is label-scanned — with two claimants the next reboot (or the
// next update run) may adopt the wrong disk. The fix is
// operational, not coded: detach the target before rebooting this
// host or running update tooling on it. Best-effort (a missing
// blkid never fails the install).
func warnForeignLabels(target string) {
	for _, label := range []string{"var", "EFI"} {
		out, err := exec.Command("blkid", "-o", "device", "-t", "LABEL="+label).Output()
		if err != nil && len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			dev := strings.TrimSpace(line)
			if dev == "" || dev == target || mountSourceMatchesTarget(dev, target) {
				continue
			}
			fmt.Fprintf(os.Stderr, "WARNING: LABEL=%s also present on %s: detach %s before rebooting this host or running update tooling on it\n", label, dev, target)
		}
	}
}
