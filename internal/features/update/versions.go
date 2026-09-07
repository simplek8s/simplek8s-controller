package update

import (
	"regexp"
	"strconv"
)

// Release file naming (PLAN-M2 3.6, ported from the simplek8s-update
// release grammar): simplek8s.<ts>.<arch>.<component>[.<compression>],
// where the controller only consumes kernel releases, e.g.
// simplek8s.202608291203.x86-64.efi.zst. The .efi image is the
// kernel+initrd; .kernel was the pre-2024 legacy name. The repo also
// serves the uncompressed .efi (and .img), but the controller downloads
// the .zst form on purpose (bandwidth).
var kernelReleaseRe = regexp.MustCompile(`^simplek8s\.([0-9]+)\.([A-Za-z0-9_-]+)\.efi\.(zst|xz)$`)

// ParseKernelRelease parses a release filename into (ts, arch). Only the
// compressed kernel artifacts (.efi.zst / .efi.xz) match; the
// uncompressed .efi is served by the repo but not consumed. Non-kernel
// components (img, info.json) do not match.
func ParseKernelRelease(filename string) (ts, arch string, ok bool) {
	m := kernelReleaseRe.FindStringSubmatch(filename)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// MapArch maps a K8s nodeInfo architecture to the release naming.
func MapArch(nodeArch string) (string, bool) {
	switch nodeArch {
	case "amd64":
		return "x86-64", true
	case "arm64":
		return "aarch64", true
	}
	return "", false
}

// runningRe extracts the running release ts from a nodeInfo
// kernelVersion like "6.18.48-simplek8s-202608291203 (amd64)".
var runningRe = regexp.MustCompile(`simplek8s-([0-9]+)`)

// RunningVersion returns the ts embedded in the kernel version, or ""
// when the node does not run a simplek8s kernel.
func RunningVersion(kernelVersion string) string {
	m := runningRe.FindStringSubmatch(kernelVersion)
	if m == nil {
		return ""
	}
	return m[1]
}

// NewerTS reports whether release ts a is newer than b (timestamps
// compare numerically; unequal widths are handled).
func NewerTS(a, b string) bool {
	na, ea := strconv.ParseInt(a, 10, 64)
	nb, eb := strconv.ParseInt(b, 10, 64)
	if ea != nil || eb != nil {
		return false
	}
	return na > nb
}
