package update

import (
	"encoding/json"
	"testing"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// setBothWindows rewrites the harness ConfigMap with the given
// updates + reboots window values (the next tick reloads them).
func setBothWindows(h *updateHarness, updates, reboots string) {
	h.t.Helper()
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.url":     h.repo.URL(),
		"updates.windows": updates,
		"reboots.windows": reboots,
	})
}

func stateOf(h *updateHarness) string {
	h.t.Helper()
	raw := h.fake.NodeAnnotation("w1", nodestate.AnnState)
	if raw == "" {
		return ""
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		h.t.Fatalf("state annotation: %v", err)
	}
	return v.State
}

func requestBy(h *updateHarness) (by string, force bool) {
	h.t.Helper()
	raw := h.fake.NodeAnnotation("w1", nodestate.AnnRequest)
	if raw == "" {
		return "", false
	}
	var v struct {
		By    string `json:"by"`
		Force bool   `json:"force"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		h.t.Fatalf("request annotation: %v", err)
	}
	return v.By, v.Force
}

// --- Pod-side enqueue (decision 31) ------------------------------------------

func TestEnqueueHappyPath(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setBothWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick() // check + stage + anchor B
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want anchored B", got)
	}
	h.tick() // eligible + both windows open -> enqueue
	if got := stateOf(h); got != "requested" {
		t.Fatalf("state = %q, want requested", got)
	}
	by, force := requestBy(h)
	if by != "controller" || force {
		t.Fatalf("request = by %q force %v, want controller/false", by, force)
	}
	if h.eventCount("UpdateHeldWindow") != 0 {
		t.Fatal("no hold event expected on the open path")
	}
}

func TestEnqueueWaitsForRebootsWindow(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setBothWindows(h, `["@every 2m"]`, `[]`)
	h.tick() // staged, reboots window empty -> wait
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (waiting)", got)
	}
	if h.eventCount("UpdateHeldWindow") != 1 {
		t.Fatalf("UpdateHeldWindow = %d, want 1", h.eventCount("UpdateHeldWindow"))
	}
	setBothWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick() // window open now (no new check needed) -> enqueue
	if got := stateOf(h); got != "requested" {
		t.Fatalf("state = %q, want requested after window opens", got)
	}
}

// --- Change-triggered reconciliation (§3.7) -----------------------------------

func TestReconcileRepointsPin(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000", "202608291203"})
	setBothWindows(h, `[]`, `[]`) // fully inert: only reconciliation may act
	// Operator pin to a local version.
	h.fake.SetNode("w1", map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		false, nil, "uid-w1")
	h.fake.SetNodeInfo("w1", "6.18.48-simplek8s-202601010000 (amd64)", "amd64")
	h.tick()
	if h.store.defGoal != "202608291203" {
		t.Fatalf("DEFAULT = %q, want re-pointed at the pin (ungated)", h.store.defGoal)
	}
	if h.eventCount("UpdateGoalCorrected") != 0 {
		t.Fatal("a well-formed local pin is not a correction")
	}
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (updates inert: no enqueue)", got)
	}
}

// --- Goal validation & safe state (§3.10) -------------------------------------

func TestMalformedGoalCorrectedUngated(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "banana"},
		[]string{"202601010000"})
	setBothWindows(h, `[]`, `[]`) // fully inert — correction still runs
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q, want safe state (running)", got)
	}
	if h.eventCount("UpdateGoalCorrected") != 1 {
		t.Fatalf("UpdateGoalCorrected = %d, want 1", h.eventCount("UpdateGoalCorrected"))
	}
	if h.store.defGoal != "202601010000" {
		t.Fatalf("DEFAULT = %q, want re-pointed at the safe state", h.store.defGoal)
	}
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (correction to running is quiescent)", got)
	}
}

func TestAbsentGoalNotInIndexCorrected(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202701010000"}, // well-formed, never existed
		[]string{"202601010000"})
	setBothWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q, want corrected (not in verified index)", got)
	}
	if h.eventCount("UpdateGoalCorrected") != 1 {
		t.Fatalf("UpdateGoalCorrected = %d, want 1", h.eventCount("UpdateGoalCorrected"))
	}
	if got := h.store.stagedVersions(); len(got) != 0 {
		t.Fatalf("staged = %v, want nothing (correct, don't stage)", got)
	}
}
