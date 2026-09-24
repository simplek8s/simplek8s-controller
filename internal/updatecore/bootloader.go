package updatecore

// Bootloader entry writers (PLAN-M2 3.7 step 5), ported from the
// reference project's syslinux/rpi writers and extended for the
// distro's GRUB menu (grub/grub.cfg; syslinux stays for legacy
// images). They point the partition's bootloader default at a staged
// kernel. All operate on a mounted partition root and are pure file
// manipulation (unit-testable in temp dirs). The managed line is the
// only line ever rewritten; everything else is preserved verbatim.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BootloaderType selects the entry writer. "auto" detects from the
// partition's config files.
type BootloaderType string

const (
	BootloaderAuto     BootloaderType = "auto"
	BootloaderGrub     BootloaderType = "grub"
	BootloaderSyslinux BootloaderType = "syslinux"
	BootloaderRpi      BootloaderType = "rpi"
)

const (
	grubConfigRel     = "grub/grub.cfg"
	syslinuxConfigRel = "syslinux/syslinux.cfg"
	rpiConfigRel      = "config.txt"
)

// DetectBootloader inspects the mounted partition root and returns the
// bootloader type whose config file is present (grub wins when several
// exist: new x86-64 images ship grub only, pre-grub images syslinux
// only). No config present is an error (nothing to point at).
func DetectBootloader(partRoot string) (BootloaderType, error) {
	if fileExists(filepath.Join(partRoot, grubConfigRel)) {
		return BootloaderGrub, nil
	}
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
	case BootloaderGrub:
		return setGrubDefault(partRoot, relKernelPath)
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
	case BootloaderGrub:
		k, _ := grubDefault(partRoot)
		return k
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

// --- grub ------------------------------------------------------------
// The x86-64 GRUB menu (grub/grub.cfg) is the managed file; the
// redirect configs (EFI/BOOT/grub.cfg, EFI/debian/grub.cfg,
// boot/grub/grub.cfg) only locate the ESP and are never touched.
// Default is a NAME (the entry --id, i.e. the kernel basename without
// extension), symmetric to syslinux DEFAULT <label>:
//   set default=simplek8s.<ts>.<arch>
//   menuentry "SimpleK8s <ts> <arch>" --id simplek8s.<ts>.<arch> {
//       linux /simplek8s/simplek8s.<ts>.<arch>.efi
//   }
// New entries are inserted before the first menuentry (newest first);
// the MOK enroll entry stays last. Numeric defaults (the pre-id
// template's `set default=0`) are accepted on read (Nth menuentry) and
// normalized to --id on write.

var (
	reGrubDefault    = regexp.MustCompile(`(?i)^\s*set\s+default\s*=\s*(.+?)\s*$`)
	reGrubKernelOpts = regexp.MustCompile(`(?i)^\s*set\s+kernel_opts\s*=`)
	reGrubMenuentr   = regexp.MustCompile(`(?i)^\s*menuentry\s+(?:"([^"]+)"|'([^']+)')(.*)\{\s*$`)
	reGrubID         = regexp.MustCompile(`--id[=\s]+("[^"]+"|'[^']+'|[^\s]+)`)
	reGrubLinux      = regexp.MustCompile(`(?i)^\s*linux\s+(?P<kernel>\S+)`)
	reGrubClose      = regexp.MustCompile(`^\s*\}\s*$`)
)

// grubEntry is one parsed menuentry block.
type grubEntry struct {
	index int // 0-based among menuentries
	id    string
	linux string // first linux path in the block, "" when none
	start int    // line index of the menuentry line
	end   int    // line index of the closing brace (inclusive)
}

func stripGrubQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func grubEntryID(line string) string {
	if m := reGrubID.FindStringSubmatch(line); m != nil {
		return stripGrubQuotes(m[1])
	}
	return ""
}

func grubEntryTitle(line string) string {
	if m := reGrubMenuentr.FindStringSubmatch(line); m != nil {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	return ""
}

// parseGrubEntries enumerates the menuentry blocks in cfg lines. A block
// runs from its menuentry line to the first closing-brace-only line, or
// — when malformed — to the next menuentry line or EOF.
func parseGrubEntries(lines []string) []grubEntry {
	var starts []int
	for i, ln := range lines {
		if reGrubMenuentr.MatchString(ln) {
			starts = append(starts, i)
		}
	}
	out := make([]grubEntry, 0, len(starts))
	for n, s := range starts {
		e := len(lines)
		if n+1 < len(starts) {
			e = starts[n+1]
		}
		end := e - 1
		for i := s + 1; i < e; i++ {
			if reGrubClose.MatchString(lines[i]) {
				end = i
				break
			}
		}
		id := grubEntryID(lines[s])
		linux := ""
		for _, ln := range lines[s : end+1] {
			if m := reGrubLinux.FindStringSubmatch(ln); m != nil {
				linux = strings.Trim(m[reGrubLinux.SubexpIndex("kernel")], `"`)
				break
			}
		}
		out = append(out, grubEntry{index: n, id: id, linux: linux, start: s, end: end})
	}
	return out
}

func grubDefault(partRoot string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(partRoot, grubConfigRel))
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	entries := parseGrubEntries(lines)
	val := ""
	for _, ln := range lines {
		if m := reGrubDefault.FindStringSubmatch(ln); m != nil {
			val = stripGrubQuotes(m[1])
			break
		}
	}
	if val == "" {
		return "", nil
	}
	if n, err := strconv.Atoi(val); err == nil {
		for _, e := range entries {
			if e.index == n {
				return e.linux, nil
			}
		}
		return "", nil
	}
	for _, e := range entries {
		if e.id != "" && e.id == val {
			return e.linux, nil
		}
	}
	// Fallback: match the menuentry title.
	for _, e := range entries {
		if grubEntryTitle(lines[e.start]) == val {
			return e.linux, nil
		}
	}
	return "", nil
}

// grubMenuTitle derives the display title for a staged kernel.
func grubMenuTitle(relKernelPath string) string {
	if ts, fa, ok := ParseStoredKernel(filepath.Base(relKernelPath)); ok {
		return fmt.Sprintf("SimpleK8s %s %s", ts, fa)
	}
	return kernelBasename(relKernelPath)
}

func setGrubDefault(partRoot, relKernelPath string) error {
	cfgPath := filepath.Join(partRoot, grubConfigRel)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	entries := parseGrubEntries(lines)
	id := kernelBasename(relKernelPath)

	found := false
	for _, e := range entries {
		key := e.id
		if key == "" && e.linux != "" {
			key = kernelBasename(e.linux)
		}
		if key == id {
			found = true
			break
		}
	}
	if !found {
		block := []string{
			fmt.Sprintf(`menuentry "%s" --id %s {`, grubMenuTitle(relKernelPath), id),
			fmt.Sprintf("\tlinux %s ${kernel_opts}", relKernelPath),
			"}",
			"",
		}
		// Newest first: insert before the first menuentry, so the
		// menu reads newest-to-oldest. The MOK conditional is
		// never inserted before or into — it stays last.
		at := len(lines)
		for i, ln := range lines {
			if reGrubMenuentr.MatchString(ln) {
				at = i
				break
			}
		}
		// Keep a blank line between the header and the new block.
		if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
			block = append([]string{""}, block...)
		}
		lines = append(lines[:at], append(block, lines[at:]...)...)
		entries = parseGrubEntries(lines)
	}

	defLine := fmt.Sprintf("set default=%s", id)
	done := false
	optsPresent := false
	for i, ln := range lines {
		if reGrubKernelOpts.MatchString(ln) {
			optsPresent = true
		}
		if reGrubDefault.MatchString(ln) {
			lines[i] = defLine
			done = true
		}
	}
	if !done {
		at := 0
		for i, ln := range lines {
			if reGrubMenuentr.MatchString(ln) {
				at = i
				break
			}
		}
		lines = append(lines[:at], append([]string{defLine}, lines[at:]...)...)
	}
	if !optsPresent {
		// Operator-owned kernel args (PLAN M8 D13): the variable
		// is declared once, empty (behavior-neutral), and never
		// rewritten afterwards — new entries reference it.
		for i, ln := range lines {
			if reGrubDefault.MatchString(ln) {
				lines = append(lines[:i], append([]string{`set kernel_opts=""`}, lines[i:]...)...)
				break
			}
		}
	}

	out := strings.Join(lines, "\n")
	fo, err := os.CreateTemp("", "tmp-grub-*")
	if err != nil {
		return err
	}
	defer func() {
		fo.Close()
		os.Remove(fo.Name())
	}()
	if _, err := fo.WriteString(out); err != nil {
		return err
	}
	return copyOver(fo, cfgPath)
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
// kernel path (the syslinux LABEL / grub --id name).
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

// --- grub stale-entry prune (PLAN.md §3.9) ------------------------------

// pruneGrubEntries removes menuentry blocks for our kernels whose files
// are gone, returning the rewritten config and the pruned-block count.
// A block runs from its menuentry line to its closing-brace line (the
// mirror of the writer's block form), with one trailing blank line
// belonging to the block. Global lines (set default/timeout, the distro
// DOC header, the MOK conditional) are preserved verbatim. Candidate =
// first linux line references one of our kernel files
// (simplek8s.<ts>.<arch>.efi) whose file is gone AND the block is not
// the current default (belt-and-braces: the purge never deletes the
// default's file). Entries without a linux line (e.g. the MOK
// chainloader entry) and foreign entries (non-matching linux path) are
// never touched, file present or not.
func pruneGrubEntries(cfg, defID, flavor string, fileGone func(kernelBase string) bool) (string, int) {
	lines := strings.Split(cfg, "\n")
	entries := parseGrubEntries(lines)
	drop := make([]bool, len(lines))
	pruned := 0
	for _, e := range entries {
		key := e.id
		if key == "" && e.linux != "" {
			key = kernelBasename(e.linux)
		}
		if strings.EqualFold(key, defID) {
			continue
		}
		if e.linux == "" {
			continue
		}
		base := filepath.Base(strings.Trim(e.linux, `"`))
		if _, fa, ok := ParseStoredKernel(base); !ok || fa != flavor {
			continue // foreign entry (or foreign flavor: never touched)
		}
		if !fileGone(base) {
			continue
		}
		end := e.end
		if end+1 < len(lines) && strings.TrimSpace(lines[end+1]) == "" {
			// One trailing blank line belongs to the block.
			end++
		}
		for i := e.start; i <= end; i++ {
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

// pruneGrubFile prunes stale entries from the partition's grub.cfg
// after a purge deleted kernels (same mounted session). Non-grub
// partitions (incl. syslinux legacy and rpi) and missing configs are
// skipped silently. A prune failure never fails staging: the error is
// Warned and the next purge-triggered session retries (staging already
// succeeded — rolling it back would strand the node).
func pruneGrubFile(partRoot, dir, flavor string, log Logger) {
	if bt, err := DetectBootloader(partRoot); err != nil || bt != BootloaderGrub {
		return
	}
	cfgPath := filepath.Join(partRoot, grubConfigRel)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Warn("update: grub prune: cannot read config", "err", err)
		return
	}
	defID := ""
	if def, _ := grubDefault(partRoot); def != "" {
		defID = kernelBasename(def)
	}
	gone := func(base string) bool {
		return !fileExists(filepath.Join(partRoot, dir, base))
	}
	rewritten, n := pruneGrubEntries(string(raw), defID, flavor, gone)
	if n == 0 {
		return
	}
	tmp, err := os.CreateTemp("", "tmp-grub-prune-*")
	if err != nil {
		log.Warn("update: grub prune: temp file", "err", err)
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := tmp.WriteString(rewritten); err != nil {
		log.Warn("update: grub prune: temp write", "err", err)
		return
	}
	if err := copyOver(tmp, cfgPath); err != nil {
		log.Warn("update: grub prune failed after successful staging; will retry on the next purge", "err", err)
		return
	}
	log.Info("update: grub stale entries pruned", "entries", n)
}
