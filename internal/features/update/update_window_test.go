package update

import (
	"strings"
	"testing"
	"time"
)

// setWindows rewrites the harness ConfigMap with the given
// updates.windows value (the next tick reloads it).
func setWindows(h *updateHarness, windows string) {
	h.t.Helper()
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.mode":    "full",
		"updates.url":     h.repo.URL(),
		"updates.windows": windows,
	})
}

func lastCheck(h *updateHarness) string {
	h.t.Helper()
	return h.fake.NodeAnnotation("w1", "simplek8s.org/update-last-check")
}

// --- Master switch (PLAN.md §3.4) -------------------------------------------

func TestMasterSwitchEmptyWindowsIsInert(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setWindows(h, `[]`)
	h.tick()
	if h.repo.hits() != 0 {
		t.Fatalf("explicit [] must not check: %d hits", h.repo.hits())
	}
	if got := h.store.stagedVersions(); len(got) != 0 {
		t.Fatalf("explicit [] must not stage: %v", got)
	}
	if lastCheck(h) != "" {
		t.Fatalf("explicit [] must not claim: %q", lastCheck(h))
	}
	if h.eventCount("UpdateAvailable") != 0 || h.eventCount("UpdateStaged") != 0 {
		t.Fatal("explicit [] must fire no update events")
	}
}

func TestMasterSwitchClosedWindowSkips(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	// Harness clock is Saturday 2026-09-05 12:00 UTC: @daily (midnight
	// + 5m grace) is closed.
	setWindows(h, `["@daily"]`)
	h.tick()
	h.tick()
	if h.repo.hits() != 0 {
		t.Fatalf("closed window must not check: %d hits", h.repo.hits())
	}
	// Rate-limited log: one Info on the closed edge, Debug afterwards.
	infos := 0
	skips := 0
	for _, line := range strings.Split(h.logs(), "\n") {
		if strings.Contains(line, "skipping check") {
			skips++
			if strings.Contains(line, "level=INFO") {
				infos++
			}
		}
	}
	if skips != 2 {
		t.Fatalf("skip log lines = %d, want 2", skips)
	}
	if infos != 1 {
		t.Fatalf("Info skip lines = %d, want 1 (edge-triggered)", infos)
	}
}

// --- One check per occurrence (decision 21) ----------------------------------

func TestOneCheckPerOccurrencePersisted(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if h.repo.hits() != 1 {
		t.Fatalf("hits = %d, want 1", h.repo.hits())
	}
	// The claim is persisted as the occurrence start (12:00), before
	// the fetch — a pod restart re-reads it instead of re-checking.
	if got := lastCheck(h); got != "2026-09-05T12:00:00Z" {
		t.Fatalf("update-last-check = %q, want the 12:00 occurrence", got)
	}
	h.tick()
	if h.repo.hits() != 1 {
		t.Fatalf("hits after 2nd tick = %d, want 1 (same occurrence)", h.repo.hits())
	}
	// Next occurrence: a new check.
	h.cl.Advance(2 * time.Minute)
	h.tick()
	if h.repo.hits() != 2 {
		t.Fatalf("hits after next occurrence = %d, want 2", h.repo.hits())
	}
	if got := lastCheck(h); got != "2026-09-05T12:02:00Z" {
		t.Fatalf("update-last-check = %q, want the 12:02 occurrence", got)
	}
}

func TestCoincidentSchedulesShareClaim(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	// @daily and @every 24h coincide at midnight: one shared claim.
	setWindows(h, `["@daily", "@every 24h"]`)
	h.cl.Advance(12 * time.Hour) // Sunday 00:00: coincident occurrence, window open
	h.tick()
	if h.repo.hits() != 1 {
		t.Fatalf("hits = %d, want 1 (shared claim)", h.repo.hits())
	}
	if got := lastCheck(h); got != "2026-09-06T00:00:00Z" {
		t.Fatalf("update-last-check = %q, want the shared midnight claim", got)
	}
	h.cl.Advance(2 * time.Minute) // window still open, no newer occurrence
	h.tick()
	if h.repo.hits() != 1 {
		t.Fatalf("hits = %d, want 1 (shared claim covers both schedules)", h.repo.hits())
	}
}

func TestFailedCheckConsumesOccurrence(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	forger := newTestKey(t, true)
	forge := newCountingRepo(t, forger)
	forge.set(t, kernelIndex())
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.mode":    "full",
		"updates.url":     forge.URL(),
		"updates.windows": `["@every 2m"]`,
	})
	h.tick() // check fails (bad signature), occurrence consumed
	if h.repo.hits() != 0 || forge.hits() != 1 {
		t.Fatalf("hits = %d/%d, want failed check on forge only", h.repo.hits(), forge.hits())
	}
	h.tick() // same occurrence: no retry
	if forge.hits() != 1 {
		t.Fatalf("forge hits = %d, want 1 (no retry within occurrence)", forge.hits())
	}
	h.cl.Advance(2 * time.Minute)
	h.tick() // next occurrence: retried
	if forge.hits() != 2 {
		t.Fatalf("forge hits = %d, want 2 (next occurrence retries)", forge.hits())
	}
}

func TestCorruptClaimTreatedAsAbsent(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel":       "202601010000",
			"simplek8s.org/update-last-check": "garbage",
		},
		[]string{"202601010000"})
	h.tick()
	if h.repo.hits() != 1 {
		t.Fatalf("hits = %d, want 1 (corrupt claim = absent)", h.repo.hits())
	}
	if got := lastCheck(h); got != "2026-09-05T12:00:00Z" {
		t.Fatalf("update-last-check = %q, want overwritten with occurrence", got)
	}
}

func TestStoredNewerClaimSkips(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel":       "202601010000",
			"simplek8s.org/update-last-check": "2026-09-05T12:02:00Z", // a later occurrence
		},
		[]string{"202601010000"})
	h.tick()
	if h.repo.hits() != 0 {
		t.Fatalf("hits = %d, want 0 (stored claim covers now)", h.repo.hits())
	}
}
