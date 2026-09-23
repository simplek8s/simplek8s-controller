package updatecore

import (
	"regexp"
)

// Nodectl-install IMG naming (PLAN.md §3.15, M8 D9): the publisher
// serves the install images as simplek8s.<ts>.<arch>.img.zst (the
// bandwidth form — plain `.img` and `latest` aliases exist in the
// index but are never consumed, same rule as ParseKernelRelease).
// The ts is exactly 12 digits (YYYYMMDDHHMM, the release stamp):
// the index carries two Feb-2023 legacy entries with longer
// numerics that must never win newest-TS selection.
var imgReleaseRe = regexp.MustCompile(`^simplek8s\.([0-9]{12})\.([A-Za-z0-9_-]+)\.img\.zst$`)

// ParseImgRelease parses a release filename into (ts, arch). Only
// the compressed `.img.zst` artifacts match.
func ParseImgRelease(filename string) (ts, arch string, ok bool) {
	m := imgReleaseRe.FindStringSubmatch(filename)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// FilterImgIndex resolves the newest indexed install image for
// arch (same selection rule as FilterIndexByFlavor — the publisher
// alias is neither trusted nor needed).
func FilterImgIndex(sums map[string]string, arch string) (ts, file, checksum string, ok bool) {
	var bestTS, bestFile, bestSum string
	found := false
	for file := range sums {
		ts, fa, ok := ParseImgRelease(file)
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
