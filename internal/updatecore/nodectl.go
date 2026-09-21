package updatecore

import (
	"regexp"
)

// Nodectl release file naming (PLAN.md §3.14): the publisher serves
// the uploaded CLI binaries under a signed SHA256SUMS as
// nodectl.<ts>.<arch> (e.g. nodectl.202609171200.x86-64), with
// <ts> from the nodectl build (`make build-nodectl` TS). The
// `latest`-style aliases carry no ts and never match — same rule as
// ParseStoredKernel.
var nodectlReleaseRe = regexp.MustCompile(`^nodectl\.([0-9]+)\.([A-Za-z0-9_-]+)$`)

// ParseNodectlRelease parses a published nodectl filename into
// (ts, arch). Non-matching names (aliases, foreign files) report
// ok=false.
func ParseNodectlRelease(filename string) (ts, arch string, ok bool) {
	m := nodectlReleaseRe.FindStringSubmatch(filename)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// NodectlArch maps a Go build arch to the release channel infix
// (PLAN.md §3.14, shared with the kernel artifact vocabulary —
// §4.6 D6): amd64 runs the x86-64 binary, arm64 the arm64 one.
// Anything else reports ok=false: the caller fails closed, never
// downloads a foreign-arch binary.
func NodectlArch(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x86-64", true
	case "arm64":
		return "arm64", true
	default:
		return "", false
	}
}

// FilterNodectlIndex resolves the newest indexed nodectl release for
// arch (PLAN.md §3.14: computed max TS, same selection rule as
// FilterIndexByFlavor — the publisher alias is neither trusted nor
// needed).
func FilterNodectlIndex(sums map[string]string, arch string) (ts, file, checksum string, ok bool) {
	var bestTS, bestFile, bestSum string
	found := false
	for file := range sums {
		ts, fa, ok := ParseNodectlRelease(file)
		if !ok || fa != arch {
			continue
		}
		if !found || NewerTS(ts, bestTS) {
			bestTS, bestFile, bestSum = ts, file, sums[file]
			found = true
		}
	}
	if !found {
		return "", "", "", false
	}
	return bestTS, bestFile, bestSum, true
}
