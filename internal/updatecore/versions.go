package updatecore

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

// MapArch is retired (PLAN-M5): it translated node `arm64` to
// `aarch64`, a flavor the repo never shipped, so every arm64 node
// silently saw no updates. Flavor now resolves from staged files
// (ResolveFlavor), not from node architecture.

// FlavorSet is the closed set of release artifact flavors
// (PLAN-M5 §3.1, D-table). Anything else never resolves.
var flavorSet = map[string]bool{
	"x86-64": true,
	"arm64":  true, // legacy pre-rpi5 naming (ended lineage)
	"rpi4":   true,
	"rpi5":   true,
}

// storedKernelFlavorRe parses a staged kernel basename into ts +
// flavor: simplek8s.<ts>.<flavor>.efi (same shape as storedKernelRe,
// also yielding the arch group).
var storedKernelFlavorRe = regexp.MustCompile(`^simplek8s\.([0-9]+)\.([A-Za-z0-9_-]+)\.efi$`)

// ParseStoredKernel parses a staged kernel basename into its release
// ts and artifact flavor. Non-matching names (foreign files,
// `latest`-style aliases) report ok=false.
func ParseStoredKernel(filename string) (ts, flavor string, ok bool) {
	m := storedKernelFlavorRe.FindStringSubmatch(filename)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// IsKnownFlavor reports whether f is a shippable release flavor
// (PLAN-M5 §3.1 closed set, incl. the ended legacy arm64 lineage).
func IsKnownFlavor(f string) bool {
	return flavorSet[f]
}

// ResolveFlavor infers the node's flavor from staged kernel
// basenames: the flavor of the first versioned file wins (directory
// order is sorted, so this is deterministic). Unknown flavors are
// skipped; ok==false when no file carries a known flavor (empty or
// foreign-only partition → D2 fail-closed upstream).
func ResolveFlavor(files []string) (string, bool) {
	for _, f := range files {
		if _, flavor, ok := ParseStoredKernel(f); ok && flavorSet[flavor] {
			return flavor, true
		}
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
