package update

// PhysicalStore is the real BootStore (PLAN-M2 3.7): it discovers the
// boot device over the host /dev, mounts it at a private mountpoint with
// syscall.Mount, and delegates the staging/scan to the pure
// stagePartition/listPartitionVersions. The device discovery, mount and
// unmount are injectable so the store is unit-testable without loop
// devices; the real paths use blkid(8) (util-linux, present in the image)
// and the kernel mount(2) syscall.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// PhysicalStoreConfig tunes the physical boot store.
type PhysicalStoreConfig struct {
	// DevRoot is the directory holding the host block devices
	// (default "/dev"; M5 points it at the host /dev hostPath).
	DevRoot string
	// MountRoot is where private mountpoints are created (default
	// /run/simplek8s-controller).
	MountRoot string
	// KernelDir is the partition subdir holding stored kernels (default
	// "simplek8s").
	KernelDir string
	// WorkDir is the scratch dir for download + extract (default the pod
	// temp; must NOT be the small boot partition).
	WorkDir string
	// FSType is the partition filesystem (default "vfat").
	FSType string
	// HTTPClient for artifact downloads.
	HTTPClient *http.Client
	// Log.
	Log Logger

	// Injectable seams (nil => real).
	Mount   func(device, target, fstype string) error
	Unmount func(target string) error
	Blkid   func(args ...string) (string, error)
}

// Logger is the minimal logging surface the store needs (slog.Logger
// satisfies it; tests use a stub).
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// PhysicalStore implements BootStore.
type PhysicalStore struct {
	devRoot   string
	mountRoot string
	kernelDir string
	workDir   string
	fstype    string
	http      *http.Client
	log       Logger
	mountFn   func(device, target, fstype string) error
	unmountFn func(target string) error
	blkid     func(args ...string) (string, error)

	// Verified boot device cache (PLAN-M5 §3.6): topology does not
	// change under a running pod, so the device is resolved once and
	// re-resolved from scratch only on a mount failure.
	mu         sync.Mutex
	cachedDev  string
	cacheValid bool
}

// NewPhysicalStore applies defaults and returns a ready store.
func NewPhysicalStore(cfg PhysicalStoreConfig) *PhysicalStore {
	if cfg.DevRoot == "" {
		cfg.DevRoot = "/dev"
	}
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/run/simplek8s-controller"
	}
	if cfg.KernelDir == "" {
		cfg.KernelDir = "simplek8s"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if cfg.FSType == "" {
		cfg.FSType = "vfat"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Log == nil {
		cfg.Log = discardLogger{}
	}
	if cfg.Mount == nil {
		cfg.Mount = defaultMount
	}
	if cfg.Unmount == nil {
		cfg.Unmount = defaultUnmount
	}
	if cfg.Blkid == nil {
		cfg.Blkid = realBlkid
	}
	return &PhysicalStore{
		devRoot:   cfg.DevRoot,
		mountRoot: cfg.MountRoot,
		kernelDir: cfg.KernelDir,
		workDir:   cfg.WorkDir,
		fstype:    cfg.FSType,
		http:      cfg.HTTPClient,
		log:       cfg.Log,
		mountFn:   cfg.Mount,
		unmountFn: cfg.Unmount,
		blkid:     cfg.Blkid,
	}
}

// Versions lists the release ts present on this node's boot partition.
func (s *PhysicalStore) Versions(ctx context.Context) ([]string, error) {
	mnt, cleanup, err := s.mountedBoot(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return listPartitionVersions(mnt, s.kernelDir)
}

// Kernels lists staged kernel basenames (with flavor part) for flavor
// resolution (PLAN-M5 §3.1). Non-matching files are omitted.
func (s *PhysicalStore) Kernels(ctx context.Context) ([]string, error) {
	mnt, cleanup, err := s.mountedBoot(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	entries, err := listKernels(mnt, s.kernelDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out, nil
}

// Stage stages one release onto this node's boot partition.
func (s *PhysicalStore) Stage(ctx context.Context, req StageRequest) error {
	mnt, cleanup, err := s.mountedBoot(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	work, err := os.MkdirTemp(s.workDir, "stage-")
	if err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(work)
	return stagePartition(ctx, s.http, s.log, req, mnt, s.kernelDir, work)
}

// EnsureBootGoal verifies version's kernel file is present and ensures
// the bootloader DEFAULT points at it, in one mounted session
// (PLAN.md §3.4 ordering invariant, §3.7). A matching DEFAULT is a
// no-op (compare only, still a mount). O_SYNC writers make the
// re-point durable before return. It reports whether it re-pointed.
func (s *PhysicalStore) EnsureBootGoal(ctx context.Context, version, arch string) (bool, error) {
	mnt, cleanup, err := s.mountedBoot(ctx)
	if err != nil {
		return false, err
	}
	defer cleanup()
	stored := kernelStoredName(version, arch)
	if !fileExists(filepath.Join(mnt, s.kernelDir, stored)) {
		return false, ErrGoalAbsent
	}
	want := "/" + filepath.Join(s.kernelDir, stored)
	if cur := GetBootloaderDefault(BootloaderAuto, mnt); cur == want {
		s.log.Debug("update: bootloader default already at goal", "version", version)
		return false, nil
	}
	if err := SetBootloaderDefault(BootloaderAuto, mnt, want, "/"); err != nil {
		return false, fmt.Errorf("re-point default at %s: %w", version, err)
	}
	s.log.Info("update: bootloader default re-pointed", "version", version)
	return true, nil
}

// Boot device discovery (PLAN-M5 §3.6, D2): enumerate-then-verify.
//
// A stray USB stick, a second disk's ESP, or a reused `boot` label
// elsewhere can present the same filesystem labels as our partition,
// so first-match discovery is unsafe on write paths. Instead every
// candidate is mounted and verified (it must hold the kernel dir AND
// a bootloader config); the first verifying candidate wins, a second
// one Warns loudly, and none verifying fails closed (Warn, no mounts
// left behind, nothing written anywhere). The verified device is
// cached per pod lifetime and re-resolved from scratch on any mount
// failure (see mountedBoot).
func (s *PhysicalStore) findBootDevice(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.cacheValid {
		dev := s.cachedDev
		s.mu.Unlock()
		return dev, nil
	}
	s.mu.Unlock()
	cands, err := s.candidates()
	if err != nil {
		return "", err
	}
	var verified []string
	for _, dev := range cands {
		if s.verifyCandidate(dev) {
			verified = append(verified, dev)
		}
	}
	if len(verified) == 0 {
		s.log.Warn("update: no verifying boot partition found; skipping update work (fail-closed)")
		return "", fmt.Errorf("no verifying boot device among %d candidate(s)", len(cands))
	}
	if len(verified) > 1 {
		s.log.Warn("update: multiple verifying boot partitions; using first (operator-owned ambiguity)", "device", verified[0], "others", strings.Join(verified[1:], ","))
	} else {
		s.log.Debug("update: using verified boot device", "device", verified[0])
	}
	s.mu.Lock()
	s.cachedDev = verified[0]
	s.cacheValid = true
	s.mu.Unlock()
	return verified[0], nil
}

// candidates enumerates boot device candidates in M5 §3.6 priority:
// PARTLABEL=boot first (the distro convention per AGENTS.md; GPT only
// — absent on MBR layouts like the rpi nodes), then the by-label
// symlinks (EFI, boot), then the blkid export scan (PARTLABEL=boot,
// LABEL=EFI, LABEL=boot). Entries resolving to the same device
// collapse (first position wins).
func (s *PhysicalStore) candidates() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(dev string) {
		if dev == "" {
			return
		}
		key := canonicalKey(dev)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, dev)
	}
	if p := filepath.Join(s.devRoot, "disk", "by-partlabel", "boot"); isSymlink(p) {
		add(p)
	}
	for _, lbl := range []string{"EFI", "boot"} {
		if p := filepath.Join(s.devRoot, "disk", "by-label", lbl); isSymlink(p) {
			add(p)
		}
	}
	blkout, err := s.blkid("-o", "export")
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("no boot device under %s (blkid: %w)", s.devRoot, err)
	}
	for _, dev := range parseBlkidCandidates(blkout) {
		add(dev)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no labeled boot device (PARTLABEL=boot, EFI/boot) under %s", s.devRoot)
	}
	return out, nil
}

// isSymlink reports whether p exists and is a symlink.
func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// canonicalKey maps a candidate path to its dedup key: a symlink's
// target (as written by udev, relative or absolute), else the path
// itself. It reads the link only, so dangling test links still key
// deterministically.
func canonicalKey(p string) string {
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(p); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			return filepath.Clean(target)
		}
	}
	return filepath.Clean(p)
}

// verifyCandidate mounts dev and reports whether it holds our boot
// partition contents: the kernel dir AND a bootloader config (an empty
// partition fails verification — it heals with one manual
// `mkdir simplek8s`). The mount is read-only in effect: verification
// performs zero writes, so the regular mount path is reused (no extra
// seam for the M6 CLI to carry). The mount is always released before
// return — a failed candidate leaves nothing behind.
func (s *PhysicalStore) verifyCandidate(dev string) bool {
	mnt, cleanup, err := s.mountDevice(dev)
	if err != nil {
		s.log.Debug("update: boot candidate mount failed; skipping", "device", dev, "err", err)
		return false
	}
	defer cleanup()
	if st, err := os.Stat(filepath.Join(mnt, s.kernelDir)); err != nil || !st.IsDir() {
		return false
	}
	if _, err := DetectBootloader(mnt); err != nil {
		return false
	}
	return true
}

// invalidate drops the cached device so the next resolution starts
// from scratch (the device went away or was never ours).
func (s *PhysicalStore) invalidate() {
	s.mu.Lock()
	s.cachedDev, s.cacheValid = "", false
	s.mu.Unlock()
}

// mountedBoot resolves the verified device and mounts it, retrying
// once from a fresh enumeration if the mount fails (stale cache after
// a device rename, or a candidate that verified and then vanished).
// Callers own the cleanup func.
func (s *PhysicalStore) mountedBoot(ctx context.Context) (string, func(), error) {
	dev, err := s.findBootDevice(ctx)
	if err != nil {
		return "", func() {}, err
	}
	if mnt, cleanup, mntErr := s.mountDevice(dev); mntErr == nil {
		return mnt, cleanup, nil
	}
	s.invalidate()
	dev, err = s.findBootDevice(ctx)
	if err != nil {
		return "", func() {}, err
	}
	return s.mountDevice(dev)
}

// mountDevice mounts dev at a fresh private mountpoint and returns the
// path plus a cleanup func (unmount + remove the mountpoint).
func (s *PhysicalStore) mountDevice(dev string) (string, func(), error) {
	// os.MkdirTemp does not create parents; the mount root (pod /run, a
	// writable tmpfs) may not exist yet. Ensure it before making the
	// private mountpoint, or every scan/stage fails silently.
	if err := os.MkdirAll(s.mountRoot, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("mount root %s: %w", s.mountRoot, err)
	}
	target, err := os.MkdirTemp(s.mountRoot, "mnt-")
	if err != nil {
		return "", func() {}, fmt.Errorf("mountpoint: %w", err)
	}
	s.log.Debug("update: mounting boot device", "device", dev, "target", target, "fstype", s.fstype)
	mounted := false
	cleanup := func() {
		if mounted {
			if uerr := s.unmountFn(target); uerr != nil {
				s.log.Warn("unmount failed", "target", target, "err", uerr)
			}
		}
		os.RemoveAll(target)
	}
	if err := s.mountFn(dev, target, s.fstype); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("mount %s: %w", dev, err)
	}
	mounted = true
	return target, cleanup, nil
}

// parseBlkidCandidates lists devices from `blkid -o export` output in
// M5 §3.6 priority: PARTLABEL=boot first, then LABEL=EFI, then
// LABEL=boot (each in appearance order). A device carrying
// PARTLABEL=boot wins its bucket regardless of its LABEL. Pure
// (unit-tested).
func parseBlkidCandidates(out string) []string {
	type rec struct {
		dev   string
		part  string
		label string
	}
	var recs []rec
	cur := rec{}
	flush := func() {
		if cur.dev != "" {
			recs = append(recs, cur)
		}
		cur = rec{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k, v := line[:eq], strings.Trim(line[eq+1:], `"`)
		switch k {
		case "DEVNAME":
			flush()
			cur.dev = v
		case "PARTLABEL":
			if cur.dev != "" {
				cur.part = v
			}
		case "LABEL":
			if cur.dev != "" {
				cur.label = v
			}
		}
	}
	flush()
	var part, efi, boot []string
	for _, r := range recs {
		switch {
		case r.part == "boot":
			part = append(part, r.dev)
		case r.label == "EFI":
			efi = append(efi, r.dev)
		case r.label == "boot":
			boot = append(boot, r.dev)
		}
	}
	return append(append(part, efi...), boot...)
}

// parseBlkid finds the first device with LABEL=EFI (preferred) or
// LABEL=boot in `blkid -o export` output. Pure (unit-tested).
func parseBlkid(out string) (string, bool) {
	var cur, efi, boot string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k, v := line[:eq], strings.Trim(line[eq+1:], `"`)
		switch k {
		case "DEVNAME":
			cur = v
		case "LABEL":
			if cur == "" {
				continue
			}
			switch v {
			case "EFI":
				if efi == "" {
					efi = cur
				}
			case "boot":
				if boot == "" {
					boot = cur
				}
			}
		}
	}
	if efi != "" {
		return efi, true
	}
	if boot != "" {
		return boot, true
	}
	return "", false
}

// --- real system seams ------------------------------------------------

func defaultMount(device, target, fstype string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	return syscall.Mount(device, target, fstype, 0, "")
}

func defaultUnmount(target string) error {
	return syscall.Unmount(target, 0)
}

// realBlkid runs blkid(8) with the given args (util-linux, in the image).
func realBlkid(args ...string) (string, error) {
	out, err := exec.Command("blkid", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("blkid %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
