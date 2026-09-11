package update

// Bootloader entry writers (PLAN-M2 3.7 step 5), ported from the
// reference project's syslinux/rpi writers. They point the partition's
// bootloader default at a staged kernel. All operate on a mounted
// partition root and are pure file manipulation (unit-testable in temp
// dirs). The managed line is the only line ever rewritten; everything
// else is preserved verbatim.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BootloaderType selects the entry writer. "auto" detects from the
// partition's config files.
type BootloaderType string

const (
	BootloaderAuto     BootloaderType = "auto"
	BootloaderSyslinux BootloaderType = "syslinux"
	BootloaderRpi      BootloaderType = "rpi"
)

const (
	syslinuxConfigRel = "syslinux/syslinux.cfg"
	rpiConfigRel      = "config.txt"
)

// DetectBootloader inspects the mounted partition root and returns the
// bootloader type whose config file is present (syslinux wins when both
// exist). No config present is an error (nothing to point at).
func DetectBootloader(partRoot string) (BootloaderType, error) {
	if fileExists(filepath.Join(partRoot, syslinuxConfigRel)) {
		return BootloaderSyslinux, nil
	}
	if fileExists(filepath.Join(partRoot, rpiConfigRel)) {
		return BootloaderRpi, nil
	}
	return "", fmt.Errorf("no bootloader config found under %s", partRoot)
}

// SetBootloaderDefault points the partition's bootloader default at
// relKernelPath (rooted at the partition root, e.g.
// /simplek8s/simplek8s.<ts>.<arch>.efi).
func SetBootloaderDefault(bootType BootloaderType, partRoot, relKernelPath, relUcode string) error {
	switch resolveBootloader(bootType) {
	case BootloaderSyslinux:
		return setSyslinuxDefault(partRoot, relKernelPath, relUcode)
	case BootloaderRpi:
		return setRPIDefault(partRoot, relKernelPath)
	default:
		detected, err := DetectBootloader(partRoot)
		if err != nil {
			return err
		}
		return SetBootloaderDefault(detected, partRoot, relKernelPath, relUcode)
	}
}

// GetBootloaderDefault returns the kernel path (relative to the
// partition root) the bootloader currently defaults to, or "" when it
// cannot be determined. Used for the "never delete the current default"
// purge guard.
func GetBootloaderDefault(bootType BootloaderType, partRoot string) string {
	switch resolveBootloader(bootType) {
	case BootloaderSyslinux:
		k, _ := syslinuxDefault(partRoot)
		return k
	case BootloaderRpi:
		k, _ := rpiDefault(partRoot)
		return k
	default:
		detected, err := DetectBootloader(partRoot)
		if err != nil {
			return ""
		}
		return GetBootloaderDefault(detected, partRoot)
	}
}

func resolveBootloader(t BootloaderType) BootloaderType {
	if t == "" || t == BootloaderAuto {
		return BootloaderAuto
	}
	return t
}

// --- syslinux ---------------------------------------------------------

func setSyslinuxDefault(partRoot, relKernelPath, relUcode string) error {
	cfgPath := filepath.Join(partRoot, syslinuxConfigRel)
	if relUcode == "" {
		relUcode = "/"
	}

	fo, err := os.CreateTemp("", "tmp-syslinux-*")
	if err != nil {
		return err
	}
	defer func() {
		fo.Close()
		os.Remove(fo.Name())
	}()

	if err := writeNewSyslinuxConfig(fo, partRoot, cfgPath, relKernelPath, relUcode); err != nil {
		return err
	}
	return copyOver(fo, cfgPath)
}

var (
	reSysDefault = regexp.MustCompile(`(?i)^DEFAULT `)
)

func writeNewSyslinuxConfig(fo *os.File, partRoot, cfgPath, relKernelPath, relUcode string) error {
	kernelName := kernelBasename(relKernelPath)
	reLabel := regexp.MustCompile(`(?i)^LABEL ` + strings.ReplaceAll(kernelName, `.`, `\.`))

	f, err := os.OpenFile(cfgPath, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	foundDefault, foundLabel := false, false
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if reSysDefault.MatchString(line) {
			if _, err := fo.WriteString(fmt.Sprintf("DEFAULT %s\n", kernelName)); err != nil {
				return err
			}
			foundDefault = true
			continue
		}
		if reLabel.MatchString(line) {
			foundLabel = true
		}
		if _, err := fo.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	if !foundDefault {
		if _, err := fo.WriteString(fmt.Sprintf("DEFAULT %s\n", kernelName)); err != nil {
			return err
		}
	}
	if !foundLabel {
		if _, err := fo.WriteString(fmt.Sprintf("LABEL %s\n KERNEL %s\n", kernelName, relKernelPath)); err != nil {
			return err
		}
		for _, uc := range []string{"intel-ucode.img", "amd-ucode.img"} {
			if fileExists(filepath.Join(partRoot, relUcode, uc)) {
				if _, err := fo.WriteString(fmt.Sprintf(" INITRD %s\n", filepath.Join(relUcode, uc))); err != nil {
					return err
				}
			}
		}
		if _, err := fo.WriteString("\n"); err != nil {
			return err
		}
	}
	return nil
}

func syslinuxDefault(partRoot string) (string, error) {
	cfgPath := filepath.Join(partRoot, syslinuxConfigRel)
	f, err := os.OpenFile(cfgPath, os.O_RDONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	label, err := syslinuxLabel(f)
	if err != nil || label == "" {
		return "", err
	}
	return syslinuxKernelForLabel(f, label)
}

func syslinuxLabel(f *os.File) (string, error) {
	re := regexp.MustCompile(`(?i)^DEFAULT\s+(?P<label>[\w-_.]+)`)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m := re.FindStringSubmatch(s.Text()); m != nil {
			return m[re.SubexpIndex("label")], nil
		}
	}
	return "", s.Err()
}

func syslinuxKernelForLabel(f *os.File, label string) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	reLabel := regexp.MustCompile(`(?i)^LABEL\s+(?P<label>[\w-_.]+)\s*$`)
	reKernel := regexp.MustCompile(`(?i)^\s*KERNEL\s+(?P<kernel>.+?)\s*$`)
	s := bufio.NewScanner(f)
	inBlock := false
	for s.Scan() {
		line := s.Text()
		if lm := reLabel.FindStringSubmatch(line); lm != nil {
			inBlock = lm[reLabel.SubexpIndex("label")] == label
			continue
		}
		if inBlock {
			if km := reKernel.FindStringSubmatch(line); km != nil {
				return km[reKernel.SubexpIndex("kernel")], nil
			}
		}
	}
	return "", s.Err()
}

// --- rpi --------------------------------------------------------------

func setRPIDefault(partRoot, relKernelPath string) error {
	cfgPath := filepath.Join(partRoot, rpiConfigRel)

	fo, err := os.CreateTemp("", "tmp-rpi-*")
	if err != nil {
		return err
	}
	defer func() {
		fo.Close()
		os.Remove(fo.Name())
	}()

	fi, err := os.OpenFile(cfgPath, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}
	reKernel := regexp.MustCompile(`(?i)^kernel=`)
	s := bufio.NewScanner(fi)
	for s.Scan() {
		line := s.Text()
		if reKernel.MatchString(line) {
			if _, err := fo.WriteString(fmt.Sprintf("kernel=%s\n", relKernelPath)); err != nil {
				fi.Close()
				return err
			}
			continue
		}
		if _, err := fo.WriteString(line + "\n"); err != nil {
			fi.Close()
			return err
		}
	}
	if err := fi.Close(); err != nil {
		return err
	}
	return copyOver(fo, cfgPath)
}

func rpiDefault(partRoot string) (string, error) {
	cfgPath := filepath.Join(partRoot, rpiConfigRel)
	f, err := os.OpenFile(cfgPath, os.O_RDONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	re := regexp.MustCompile(`(?i)^kernel=(?P<kernelPath>[/\w-_.]+)$`)
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m := re.FindStringSubmatch(s.Text()); m != nil {
			return m[re.SubexpIndex("kernelPath")], nil
		}
	}
	return "", s.Err()
}

// kernelBasename strips the directory and the final extension from a
// kernel path (the syslinux/rpi LABEL name).
func kernelBasename(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// copyOver streams the (rewound) temp file over target, creating it
// when absent, with synchronous writes (matches the reference writer).
func copyOver(src *os.File, target string) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- syslinux stale-entry prune (PLAN.md §3.9) -----------------------------

// pruneSyslinuxEntries removes entry blocks for our kernels whose files
// are gone, returning the rewritten config and the pruned-block count.
// A block runs from its LABEL line to (not including) the next LABEL
// line or EOF — the mirror of the writer's block form — with trailing
// blank lines belonging to the block. Global lines (everything before
// the first LABEL, incl. the distro DOC header) are preserved verbatim.
// Candidate = first KERNEL line references one of our kernel files
// (simplek8s.<ts>.<arch>.efi) whose file is gone AND the block is not
// the current DEFAULT (belt-and-braces: the purge never deletes the
// default's file). Foreign entries (non-matching KERNEL) are never
// touched, file present or not; blocks without a KERNEL line are never
// touched.
func pruneSyslinuxEntries(cfg, defLabel, flavor string, fileGone func(kernelBase string) bool) (string, int) {
	lines := strings.Split(cfg, "\n")
	starts := []int{} // indexes of LABEL lines
	for i, ln := range lines {
		if reSysLabel.MatchString(ln) {
			starts = append(starts, i)
		}
	}
	drop := make([]bool, len(lines))
	pruned := 0
	for b, s := range starts {
		e := len(lines)
		if b+1 < len(starts) {
			e = starts[b+1]
		}
		label := sysLabelName(lines[s])
		if strings.EqualFold(label, defLabel) {
			continue
		}
		kernel := ""
		for _, ln := range lines[s:e] {
			if m := reSysKernel.FindStringSubmatch(ln); m != nil {
				kernel = m[reSysKernel.SubexpIndex("kernel")]
				break
			}
		}
		if kernel == "" {
			continue
		}
		base := filepath.Base(strings.Trim(kernel, `"`))
		if _, fa, ok := ParseStoredKernel(base); !ok || fa != flavor {
			continue // foreign entry (or foreign flavor: never touched)
		}
		if !fileGone(base) {
			continue
		}
		for i := s; i < e; i++ {
			drop[i] = true
		}
		pruned++
	}
	if pruned == 0 {
		return cfg, 0
	}
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if !drop[i] {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n"), pruned
}

var (
	reSysLabel  = regexp.MustCompile(`(?i)^LABEL\s+(?P<label>[\w-_.]+)`)
	reSysKernel = regexp.MustCompile(`(?i)^\s*KERNEL\s+(?P<kernel>.+?)\s*$`)
)

// sysLabelName returns the label name of a LABEL line.
func sysLabelName(line string) string {
	if m := reSysLabel.FindStringSubmatch(line); m != nil {
		return m[reSysLabel.SubexpIndex("label")]
	}
	return ""
}

// pruneSyslinuxFile prunes stale entries from the partition's
// syslinux.cfg after a purge deleted kernels (same mounted session).
// Non-syslinux partitions (incl. rpi) and missing configs are skipped
// silently. A prune failure never fails staging: the error is Warned
// and the next purge-triggered session retries (staging already
// succeeded — rolling it back would strand the node).
func pruneSyslinuxFile(partRoot, dir, flavor string, log Logger) {
	if bt, err := DetectBootloader(partRoot); err != nil || bt != BootloaderSyslinux {
		return
	}
	cfgPath := filepath.Join(partRoot, syslinuxConfigRel)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Warn("update: syslinux prune: cannot read config", "err", err)
		return
	}
	defLabel := ""
	if f, err := os.Open(cfgPath); err == nil {
		defLabel, _ = syslinuxLabel(f)
		f.Close()
	}
	gone := func(base string) bool {
		return !fileExists(filepath.Join(partRoot, dir, base))
	}
	rewritten, n := pruneSyslinuxEntries(string(raw), defLabel, flavor, gone)
	if n == 0 {
		return
	}
	tmp, err := os.CreateTemp("", "tmp-syslinux-prune-*")
	if err != nil {
		log.Warn("update: syslinux prune: temp file", "err", err)
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := tmp.WriteString(rewritten); err != nil {
		log.Warn("update: syslinux prune: temp write", "err", err)
		return
	}
	if err := copyOver(tmp, cfgPath); err != nil {
		log.Warn("update: syslinux prune failed after successful staging; will retry on the next purge", "err", err)
		return
	}
	log.Info("update: syslinux stale entries pruned", "entries", n)
}
