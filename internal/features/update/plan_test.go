package update

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

const planCMName = "simplek8s-update-plans"

type planHarness struct {
	t    *testing.T
	fake *kubetest.FakeAPI
	f    *Feature
	cl   *clock
}

// newPlanHarness builds a leader-capable feature against an empty cluster.
// Only the leader (plan) path is exercised; the local staging path is not
// driven (tests call f.Run directly, like the reboot orchestrator tests).
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
		NodeName:               "w1",
		EventNamespace:         "default",
		Features:               eng.FeatureConfig,
		Store:                  &fakeStore{},
		PlanConfigMapNamespace: "default",
		PlanConfigMapName:      planCMName,
		Now:                    cl.Now,
		Log:                    log,
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

// plans reads the leader plan ConfigMap back as a plans map (nil if absent).
func (h *planHarness) plans() map[string]PlanEntry {
	data := h.fake.ConfigMapData("default", planCMName)
	if data == nil {
		return nil
	}
	var m map[string]PlanEntry
	if raw, ok := data["plans"]; ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			h.t.Fatalf("plans payload invalid: %v", err)
		}
	}
	return m
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

// staged is the common "fresh full-mode stage" annotations for version v on
// a node still running the old kernel: anchored to v and carrying the
// reboot-eligible plan trigger (not yet quiescent, since v != running).
func stagedAnn(v string) map[string]string {
	return map[string]string{
		nodestate.AnnNextKernel:     v,
		nodestate.AnnRebootEligible: v,
	}
}

// TestPlanStartsAndAdmitsMembers: two quiescent-pending nodes carrying the
// same fresh trigger start one plan, and both are enqueued into the M1
// reboot queue (reboot-state := requested, PDB-gated, not forced).
func TestPlanStartsAndAdmitsMembers(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	h.node("w1", old, stagedAnn(v))
	h.node("w2", old, stagedAnn(v))
	h.leader()
	h.orch()

	plans := h.plans()
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want exactly one active plan", plans)
	}
	e, ok := plans[v]
	if !ok {
		t.Fatalf("plan for %s missing: %+v", v, plans)
	}
	if len(e.Nodes) != 2 || e.Nodes[0] != "w1" || e.Nodes[1] != "w2" {
		t.Fatalf("members = %v, want [w1 w2]", e.Nodes)
	}
	if e.Canceling {
		t.Fatal("fresh plan must not be canceling")
	}
	if e.StartedAt == "" {
		t.Fatal("plan has no StartedAt")
	}
	for _, n := range []string{"w1", "w2"} {
		if got := h.stateOf(n); got != nodestate.Requested {
			t.Fatalf("%s state = %q, want requested (enqueued)", n, got)
		}
	}
	if h.eventCount("UpdatePlanStarted") != 1 {
		t.Fatalf("UpdatePlanStarted = %d, want 1", h.eventCount("UpdatePlanStarted"))
	}
	// The trigger is not cleared: the members are still pending (not quiescent).
	if h.ann("w1", nodestate.AnnRebootEligible) != v {
		t.Fatal("reboot-eligible cleared too early (member not quiescent)")
	}
}

// TestPlanHappyPathSettlesAndClears: a member that reboots and comes up on
// the plan version settles; the plan entry is cleared and the trigger is
// removed once the node is quiescent.
func TestPlanHappyPathSettlesAndClears(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	h.node("w1", old, stagedAnn(v))
	h.leader()
	h.orch() // start + admit

	// w1 reboots and comes back on the plan version (completed, quiescent).
	h.node("w1", "6.18.48-simplek8s-"+v+" (amd64)", map[string]string{
		nodestate.AnnState:          nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel:     v,
		nodestate.AnnRebootEligible: v,
	})
	h.orch() // verify (running==v) + settle + clear

	if len(h.plans()) != 0 {
		t.Fatalf("plan not cleared after settle: %+v", h.plans())
	}
	if got := h.ann("w1", nodestate.AnnNextKernel); got != v {
		t.Fatalf("next-kernel = %q, want %q", got, v)
	}
	if h.ann("w1", nodestate.AnnRebootEligible) != "" {
		t.Fatal("reboot-eligible not cleared on a quiescent node")
	}
	if h.eventCount("UpdatePlanCanceled") != 0 {
		t.Fatal("a consistent member must not cancel the plan")
	}
}

// TestPlanMismatchCancelsAndResets: a member that completed but came up on a
// kernel other than the plan version cancels the plan; the member is reset
// to its (actual) running kernel and the entry clears once quiescent.
func TestPlanMismatchCancelsAndResets(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	const oldV = "202601010000"
	h := newPlanHarness(t)
	h.node("w1", old, stagedAnn(v))
	h.leader()
	h.orch() // start + admit (requested)

	// The reboot "completed" but the node fell back to the old kernel.
	h.node("w1", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.orch() // verify -> cancel -> reset to running

	if h.eventCount("UpdatePlanCanceled") != 1 {
		t.Fatalf("UpdatePlanCanceled = %d, want 1", h.eventCount("UpdatePlanCanceled"))
	}
	if got := h.ann("w1", nodestate.AnnNextKernel); got != oldV {
		t.Fatalf("next-kernel = %q, want reset to running %q", got, oldV)
	}
	// Second cycle: the reset member is quiescent -> the entry clears.
	h.orch()
	if len(h.plans()) != 0 {
		t.Fatalf("canceled plan not cleared: %+v", h.plans())
	}
}

// TestPlanFailedMemberCancelsAndResets: a member whose reboot failed cancels
// the plan and is reset to its running kernel.
func TestPlanFailedMemberCancelsAndResets(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	const oldV = "202601010000"
	h := newPlanHarness(t)
	h.node("w1", old, stagedAnn(v))
	h.leader()
	h.orch() // start + admit

	h.node("w1", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Failed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.orch() // verify -> cancel -> reset

	if h.eventCount("UpdatePlanCanceled") != 1 {
		t.Fatalf("UpdatePlanCanceled = %d, want 1", h.eventCount("UpdatePlanCanceled"))
	}
	if got := h.ann("w1", nodestate.AnnNextKernel); got != oldV {
		t.Fatalf("next-kernel = %q, want reset to running %q", got, oldV)
	}
	h.orch()
	if len(h.plans()) != 0 {
		t.Fatalf("canceled plan not cleared: %+v", h.plans())
	}
}

// TestPlanTakeoverResumes: a plan persisted by a previous leader is resumed,
// not restarted (no new UpdatePlanStarted event); un-enqueued members are
// admitted on takeover.
func TestPlanTakeoverResumes(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	// A prior leader already recorded the plan; w1 has not been enqueued yet.
	h.fake.SetConfigMap("default", planCMName, map[string]string{
		"plans": `{"` + v + `":{"startedAt":"t0","nodes":["w1"]}}`,
	})
	h.node("w1", old, stagedAnn(v))
	h.leader()
	h.orch()

	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("w1 state = %q, want requested (admitted on takeover)", got)
	}
	if h.eventCount("UpdatePlanStarted") != 0 {
		t.Fatalf("UpdatePlanStarted = %d, want 0 (takeover is not a start)", h.eventCount("UpdatePlanStarted"))
	}
	if len(h.plans()) != 1 {
		t.Fatalf("plan lost on takeover: %+v", h.plans())
	}
}

// TestPlanInFlightMemberDeferred: a member already mid-reboot (in flight) is
// not re-enqueued or reset while canceling; it settles when its state lands.
func TestPlanInFlightMemberDeferred(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	const oldV = "202601010000"
	h := newPlanHarness(t)
	h.node("w1", old, stagedAnn(v))
	h.node("w2", old, stagedAnn(v))
	h.leader()
	h.orch() // start + admit both (requested)

	// w2 fails (cancels the plan); w1 is mid-reboot (draining, in flight).
	h.node("w2", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Failed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.node("w1", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Draining, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.orch() // w2 failed -> cancel; w1 in flight -> reset deferred

	if h.eventCount("UpdatePlanCanceled") != 1 {
		t.Fatalf("UpdatePlanCanceled = %d, want 1", h.eventCount("UpdatePlanCanceled"))
	}
	// w1 is still anchored to v (not reset while in flight).
	if got := h.ann("w1", nodestate.AnnNextKernel); got != v {
		t.Fatalf("in-flight w1 next-kernel = %q, want unchanged %q", got, v)
	}
	// w2 (failed, not in flight) is reset to its running kernel.
	if got := h.ann("w2", nodestate.AnnNextKernel); got != oldV {
		t.Fatalf("w2 next-kernel = %q, want reset to %q", got, oldV)
	}
	// The plan persists while the in-flight member has not settled.
	if len(h.plans()) != 1 {
		t.Fatalf("plan should persist while a member is in flight: %+v", h.plans())
	}

	// w1 lands on the plan version: now consistent, so it is kept, and the
	// plan clears once every member is quiescent.
	h.node("w1", "6.18.48-simplek8s-"+v+" (amd64)", map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.orch()
	if len(h.plans()) != 0 {
		t.Fatalf("plan not cleared after the in-flight member settled: %+v", h.plans())
	}
	if got := h.ann("w1", nodestate.AnnNextKernel); got != v {
		t.Fatalf("consistent w1 next-kernel = %q, want kept %q", got, v)
	}
}

// TestNonPlanVerifyResetsSingleFailedBoot: a node outside any plan that
// completed a reboot but did not come up on its next-kernel is reset to its
// running kernel (single-node failure recovery, PLAN-M2 3.10).
func TestNonPlanVerifyResetsSingleFailedBoot(t *testing.T) {
	const v = "202608291203"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	const oldV = "202601010000"
	h := newPlanHarness(t)
	// No reboot-eligible trigger: this node is not part of any plan.
	h.node("w9", old, map[string]string{
		nodestate.AnnState:      nodestate.StateValue(nodestate.Completed, h.cl.Now()),
		nodestate.AnnNextKernel: v,
	})
	h.leader()
	h.orch()

	if got := h.ann("w9", nodestate.AnnNextKernel); got != oldV {
		t.Fatalf("next-kernel = %q, want reset to running %q", got, oldV)
	}
	if h.eventCount("UpdateNodeFailed") != 1 {
		t.Fatalf("UpdateNodeFailed = %d, want 1", h.eventCount("UpdateNodeFailed"))
	}
	if len(h.plans()) != 0 {
		t.Fatalf("no plan should start: %+v", h.plans())
	}
}

// TestNonPlanVerifyIgnoresFailedState: a failed (non-completed) reboot is
// left alone — the node never left its old kernel, so the operator retries.
func TestNonPlanVerifyIgnoresFailedState(t *testing.T) {
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
	if h.eventCount("UpdateNodeFailed") != 0 {
		t.Fatalf("UpdateNodeFailed = %d, want 0 for a failed reboot", h.eventCount("UpdateNodeFailed"))
	}
}

// TestClearSettledRemovesQuiescentTrigger: a node already quiescent on its
// target but still carrying the trigger has the trigger cleared (no plan
// starts for it, since it is not pending).
func TestClearSettledRemovesQuiescentTrigger(t *testing.T) {
	const v = "202601010000"
	const old = "6.18.48-simplek8s-202601010000 (amd64)"
	h := newPlanHarness(t)
	h.node("w1", old, map[string]string{
		nodestate.AnnNextKernel:     v, // == running: quiescent
		nodestate.AnnRebootEligible: v,
	})
	h.leader()
	h.orch()

	if got := h.ann("w1", nodestate.AnnRebootEligible); got != "" {
		t.Fatalf("reboot-eligible = %q, want cleared on a quiescent node", got)
	}
	if len(h.plans()) != 0 {
		t.Fatalf("a quiescent stale trigger must not start a plan: %+v", h.plans())
	}
}
