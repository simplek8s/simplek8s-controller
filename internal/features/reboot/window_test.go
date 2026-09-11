package reboot

import (
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// Closed window: "@daily" with the harness clock at Saturday
// 2026-09-05 12:00 UTC (midnight occurrence + 5m grace is long past).
const closedWindows = `["@daily"]`

func TestWindowClosedHoldsNonForced(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows})
	h.node("w1")
	h.request("w1", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("state = %q, want requested (held)", got)
	}
	if n := h.eventCount("QueueHeldWindow"); n != 1 {
		t.Fatalf("QueueHeldWindow events = %d, want 1", n)
	}
	// Rate limit: the edge fires once, not once per cycle.
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("state = %q, want requested (still held)", got)
	}
	if n := h.eventCount("QueueHeldWindow"); n != 1 {
		t.Fatalf("QueueHeldWindow events = %d, want 1 (no repeat)", n)
	}
}

func TestWindowOpenAdmits(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows})
	h.node("w1")
	h.request("w1", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("state = %q, want requested (held)", got)
	}
	// Advance into the next @daily occurrence (Sunday 00:00 UTC).
	h.cl.Advance(12 * time.Hour)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining (admitted at open window)", got)
	}
}

func TestWindowClosedAdmitsForced(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows})
	h.node("w1")
	h.request("w1", true)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining (forced bypasses the window)", got)
	}
	if n := h.eventCount("QueueHeldWindow"); n != 0 {
		t.Fatalf("QueueHeldWindow events = %d, want 0", n)
	}
}

func TestWindowMixedQueueClosed(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows, maxConcurrent: 2})
	h.node("w1")
	h.node("w2")
	h.request("w1", false)
	h.request("w2", true)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Errorf("w1 state = %q, want requested (held)", got)
	}
	if got := h.stateOf("w2"); got != nodestate.Draining {
		t.Errorf("w2 state = %q, want draining (forced proceeds)", got)
	}
}

func TestWindowInFlightUntouched(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows})
	// Forced admission despite the closed window, then the in-flight
	// lifecycle proceeds while the window stays closed (phase 1
	// untouched by windows).
	h.driveToRebooting("w1", true)
}

func TestWindowHeldPerNodeEvents(t *testing.T) {
	h := newHarness(t, harnessOpts{windows: closedWindows})
	h.node("w1")
	h.node("w2")
	h.request("w1", false)
	h.request("w2", false)
	h.leader()
	h.orch()
	if n := h.eventCount("QueueHeldWindow"); n != 2 {
		t.Fatalf("QueueHeldWindow events = %d, want 2 (one per held node)", n)
	}
}
