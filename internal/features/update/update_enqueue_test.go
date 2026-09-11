package update

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// setFullWindows rewrites the harness ConfigMap in full mode with the
// given window values (the next tick reloads them).
func setFullWindows(h *updateHarness, updates, reboots string) {
	h.t.Helper()
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.update-mode": "full",
		"updates.url":         h.repo.URL(),
		"updates.windows":     updates,
		"reboots.windows":     reboots,
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
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
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
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `[]`)
	h.tick() // staged, reboots window empty -> wait
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (waiting)", got)
	}
	if h.eventCount("UpdateHeldWindow") != 1 {
		t.Fatalf("UpdateHeldWindow = %d, want 1", h.eventCount("UpdateHeldWindow"))
	}
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick() // window open now (no new check needed) -> enqueue
	if got := stateOf(h); got != "requested" {
		t.Fatalf("state = %q, want requested after window opens", got)
	}
}

func TestEnqueueStageModeNeverEnqueues(t *testing.T) {
	h := newUpdateHarness(t, "stage", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	// NOTE: helper forces full; restore stage mode below.
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.update-mode": "stage",
		"updates.url":         h.repo.URL(),
		"updates.windows":     `["@every 2m"]`,
		"reboots.windows":     `["@every 2m"]`,
	})
	h.tick() // staged + anchored, operator-driven from here
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want anchored B", got)
	}
	h.tick()
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (stage never auto-enqueues)", got)
	}
	if h.eventCount("UpdateHeldWindow") != 0 {
		t.Fatal("no hold event in stage mode (not eligible)")
	}
}

func TestEnqueueNeverIntoAbsentFile(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.store.setStageErr(errors.New("disk full"))
	h.tick() // check ok, staging fails: file absent
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (rule 5)", got)
	}
	if h.eventCount("UpdateMismatch") != 0 {
		t.Fatal("no mismatch for a never-enqueued node")
	}
}

func TestEnqueuePinAheadThenStage(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"}, // pinned, not local
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `[]`) // reboots closed: stage only
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 1 || got[0] != "202608291203" {
		t.Fatalf("staged = %v, want the pinned version staged", got)
	}
	if got := stateOf(h); got != "" {
		t.Fatalf("state = %q, want absent (reboots window closed)", got)
	}
	if h.eventCount("UpdateGoalCorrected") != 0 {
		t.Fatal("an in-index pin must not be corrected")
	}
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick() // file now present -> enqueue without any new check/stage
	if got := stateOf(h); got != "requested" {
		t.Fatalf("state = %q, want requested", got)
	}
}

func TestSuccessiveUpdateRearmsAndEnqueues(t *testing.T) {
	// W5 core: completed(A) with DEFAULT already at A, then an operator
	// pin to staged B is the change that re-arms -> the node enqueues
	// for B.
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel": "202601010000",
			nodestate.AnnState:          nodestate.StateValue(nodestate.Completed, time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)),
		},
		[]string{"202601010000", "202608291203"})
	h.store.defGoal = "202601010000" // hardware truth: DEFAULT==A already
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick() // pod start rediscovers A: no-op observation, no re-arm
	if got := stateOf(h); got != "completed" {
		t.Fatalf("state = %q, want completed (no re-arm without a transition)", got)
	}
	// Operator pins staged B: change + re-point transition -> re-arm.
	// (SetNode replaces the node object: re-apply the node info it drops.)
	h.fake.SetNode("w1", map[string]string{
		"simplek8s.org/next-kernel": "202608291203",
		nodestate.AnnState:          nodestate.StateValue(nodestate.Completed, time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)),
	}, false, nil, "uid-w1")
	h.fake.SetNodeInfo("w1", "6.18.48-simplek8s-202601010000 (amd64)", "amd64")
	h.tick()
	if got := stateOf(h); got != "requested" {
		t.Fatalf("state = %q, want requested (re-armed, then enqueued)", got)
	}
	by, _ := requestBy(h)
	if by != "controller" {
		t.Fatalf("request by = %q, want controller", by)
	}
}

// TestRestartNeverRearmsMismatch: a pod restart (fresh memory) that
// rediscovers the applied goal must NOT re-arm a completed mismatch
// resting state — otherwise every restart auto-retries broken versions
// (decision 34 regression test).
func TestRestartNeverRearmsMismatch(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel": "202608291203",
			nodestate.AnnState:          nodestate.StateValue(nodestate.Completed, time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)),
		},
		[]string{"202601010000", "202608291203"})
	h.store.defGoal = "202608291203" // DEFAULT already at the goal
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick()
	if got := stateOf(h); got != "completed" {
		t.Fatalf("state = %q, want completed", got)
	}
	// Simulate a pod restart: fresh change memory, same annotations+disk.
	h.f.mu.Lock()
	h.f.goalKnown = false
	h.f.lastGoal = ""
	h.f.mu.Unlock()
	h.tick()
	if got := stateOf(h); got != "completed" {
		t.Fatalf("state = %q after restart, want completed (no re-arm, no enqueue)", got)
	}
}

func TestFailedNeverEnqueued(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel": "202608291203",
			nodestate.AnnState:          nodestate.StateValue(nodestate.Failed, time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)),
		},
		[]string{"202601010000", "202608291203"})
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
	h.tick()
	if got := stateOf(h); got != "failed" {
		t.Fatalf("state = %q, want failed (never auto-enqueued, never re-armed)", got)
	}
}

// --- Change-triggered reconciliation (§3.7) -----------------------------------

func TestReconcileRepointsPin(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000", "202608291203"})
	setFullWindows(h, `[]`, `[]`) // fully inert: only reconciliation may act
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
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "banana"},
		[]string{"202601010000"})
	setFullWindows(h, `[]`, `[]`) // fully inert — correction still runs
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
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202701010000"}, // well-formed, never existed
		[]string{"202601010000"})
	setFullWindows(h, `["@every 2m"]`, `["@every 2m"]`)
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
