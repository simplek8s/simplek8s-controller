package update

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

type planHarness struct {
	t    *testing.T
	fake *kubetest.FakeAPI
	f    *Feature
	cl   *clock
}

// newPlanHarness builds a leader-capable feature against an empty cluster.
// Only the leader (verification) path is exercised; the local staging path
// is not driven (tests call f.Run directly, like the reboot orchestrator
// tests).
func newPlanHarness(t *testing.T) *planHarness {
	t.Helper()
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	cl := &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{
		CredsDir:           creds.Dir,
		APIEndpoint:        fake.Server.URL,
		Identity:           "pod-x/uid-x",
		NodeName:           "w1",
		LeaseNamespace:     "default",
		LeaseName:          engine.LeaseName,
		ConfigMapNamespace: "default",
		ConfigMapName:      "simplek8s-controller",
		Now:                cl.Now,
		Sleep:              func(ctx context.Context, d time.Duration) error { return nil },
		Log:                log,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := New(eng, Config{
		NodeName:       "w1",
		EventNamespace: "default",
		Features:       eng.FeatureConfig,
		Store:          &fakeStore{},
		Now:            cl.Now,
		Log:            log,
	})
	return &planHarness{t: t, fake: fake, f: f, cl: cl}
}

func (h *planHarness) ctx() context.Context { return context.Background() }

func (h *planHarness) leader() {
	h.t.Helper()
	if s := h.f.leas.Sync(h.ctx()); s != engine.LeaseLeader {
		h.t.Fatalf("lease state = %v, want LeaseLeader", s)
	}
}

func (h *planHarness) orch() { h.f.Run(h.ctx()) }

// node registers a node with the given annotations and running kernel.
func (h *planHarness) node(name, runningKernel string, anns map[string]string) {
	h.t.Helper()
	h.fake.SetNode(name, anns, false, nil, "uid-"+name)
	if runningKernel != "" {
		h.fake.SetNodeInfo(name, runningKernel, "amd64")
	}
}

func (h *planHarness) ann(name, key string) string { return h.fake.NodeAnnotation(name, key) }

func (h *planHarness) stateOf(name string) nodestate.State {
	h.t.Helper()
	md, ok := h.fake.GetNode(name)
	if !ok {
		return ""
	}
	m, _ := md["metadata"].(map[string]any)
	anns := map[string]string{}
	if a, ok := m["annotations"].(map[string]any); ok {
		for k, v := range a {
			anns[k] = v.(string)
		}
	}
	st := nodestate.Parse(anns)
	if st.State == nil || !st.State.Present {
		return ""
	}
	return st.State.State
}

func (h *planHarness) eventCount(reason string) int {
	n := 0
	for _, e := range h.fake.Events() {
		if e["reason"] == reason {
			n++
		}
	}
	return n
}

// TestVerifyMismatchEventsWithoutReset: a node outside any plan that
// completed a reboot but did not come up on its next-kernel keeps its
// goal (no automatic reset, no auto-retry — PLAN.md §3.4) and fires a
// single rate-limited UpdateMismatch.
func TestVerifyMismatchEventsWithoutReset(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	// No reboot-eligible trigger: this node is not part of any plan.
	h.node("w9", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.leader()
	h.orch()

	if got := h.ann("w9", nodestate.AnnNextKernel); got != v {
		t.Fatalf("next-kernel = %q, want kept %q (mismatch never resets)", got, v)
	}
	if h.eventCount("UpdateMismatch") != 1 {
		t.Fatalf("UpdateMismatch = %d, want 1", h.eventCount("UpdateMismatch"))
	}
	h.orch() // still diverged: rate-limited, no second event
	if h.eventCount("UpdateMismatch") != 1 {
		t.Fatalf("UpdateMismatch = %d, want 1 (no repeat)", h.eventCount("UpdateMismatch"))
	}
}

// TestVerifyAppliedOnQuiescenceTransition: a node recorded non-quiescent
// that later rests on its goal fires exactly one UpdateApplied.
func TestVerifyAppliedOnQuiescenceTransition(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	const newKernel = "6.18.48-simplek8s-202608291203 (amd64)"
	h := newPlanHarness(t)
	h.node("w9", old, map[string]string{
		nodestate.AnnNextKernel: v,
	})
	h.leader()
	h.orch() // non-quiescent: recorded, no event
	if h.eventCount("UpdateApplied") != 0 {
		t.Fatalf("UpdateApplied = %d, want 0 (not quiescent yet)", h.eventCount("UpdateApplied"))
	}
	// The node reboots into v and rests (completed state optional).
	h.node("w9", newKernel, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.orch()
	if h.eventCount("UpdateApplied") != 1 {
		t.Fatalf("UpdateApplied = %d, want 1 (transition into quiescence)", h.eventCount("UpdateApplied"))
	}
	h.orch()
	if h.eventCount("UpdateApplied") != 1 {
		t.Fatalf("UpdateApplied = %d, want 1 (idempotent per node+version)", h.eventCount("UpdateApplied"))
	}
}

// TestVerifyIgnoresFailedState: a failed (non-completed) reboot fires no
// mismatch — the node never left its old kernel, so the operator retries.
func TestVerifyIgnoresFailedState(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	h.node("w9", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Failed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.leader()
	h.orch()

	if got := h.ann("w9", nodestate.AnnNextKernel); got != v {
		t.Fatalf("next-kernel = %q, want unchanged %q (failed is not reset)", got, v)
	}
	if h.eventCount("UpdateMismatch") != 0 {
		t.Fatalf("UpdateMismatch = %d, want 0 for a failed reboot", h.eventCount("UpdateMismatch"))
	}
}

// TestVerifyAlwaysOnWindowsEmptyAndModeOff: verification is purely
// observational — it fires even with updates.windows [] and with
// updates.windows closed (PLAN.md §3.4).
func TestVerifyAlwaysOnWindowsEmptyAndModeOff(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	mk := func(t *testing.T) *planHarness {
		h := newPlanHarness(t)
		h.node("w9", old, map[string]string{
			nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
			nodestate.AnnNextKernel: v,
		})
		return h
	}
	t.Run("empty-windows", func(t *testing.T) {
		h := mk(t)
		h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
			"updates.windows": `[]`,
		})
		h.leader()
		h.orch()
		if h.eventCount("UpdateMismatch") != 1 {
			t.Fatalf("UpdateMismatch = %d, want 1 (always on)", h.eventCount("UpdateMismatch"))
		}
	})
	t.Run("defaults", func(t *testing.T) {
		h := mk(t) // no ConfigMap: built-in defaults
		h.leader()
		h.orch()
		if h.eventCount("UpdateMismatch") != 1 {
			t.Fatalf("UpdateMismatch = %d, want 1 (events only)", h.eventCount("UpdateMismatch"))
		}
	})
}
