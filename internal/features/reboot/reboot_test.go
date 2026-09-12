package reboot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type harnessOpts struct {
	maxConcurrent int
	onFailure     string
	drainTimeout  time.Duration
	issueGrace    time.Duration
	// windows is the raw reboots.windows value; "" means an always-open
	// window so pre-window tests keep M1 admission behavior.
	windows string
}

type harness struct {
	t        *testing.T
	fake     *kubetest.FakeAPI
	cl       *clock
	f        *Feature
	labels   map[string]map[string]string
	bootID   string
	reboots  int
	startErr error
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.maxConcurrent <= 0 {
		o.maxConcurrent = 1
	}
	if o.onFailure == "" {
		o.onFailure = "pause"
	}
	if o.drainTimeout <= 0 {
		o.drainTimeout = 5 * time.Minute
	}
	if o.issueGrace <= 0 {
		o.issueGrace = 2 * time.Minute
	}
	windows := o.windows
	if windows == "" {
		windows = `["@every 1m"]`
	}
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	cl := &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	h := &harness{t: t, fake: fake, cl: cl, labels: map[string]map[string]string{}, bootID: "boot-1"}
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	// Reboot tuning comes from the flat-key ConfigMap (PLAN-M2 3.2);
	// the harness publishes it and drives one engine cycle so the
	// feature config snapshot is resolved before the test manipulates
	// nodes directly.
	fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"reboots.max-concurrent": strconv.Itoa(o.maxConcurrent),
		"reboots.on-failure":     o.onFailure,
		"reboots.drain-timeout":  o.drainTimeout.String(),
		"reboots.issue-grace":    o.issueGrace.String(),
		"reboots.windows":        windows,
	})
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
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.f = New(eng, Config{
		NodeName:   "w1",
		PodUID:     "uid-x",
		Features:   eng.FeatureConfig,
		BootIDFunc: func() (string, error) { return h.bootID, nil },
		StartReboot: func(ctx context.Context) error {
			h.reboots++
			return h.startErr
		},
		Now: cl.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	eng.Cycle(context.Background()) // resolves the feature config snapshot
	return h
}

func (h *harness) ctx() context.Context { return context.Background() }

// leader acquires the lease so orchestrator admission may run.
func (h *harness) leader() {
	h.t.Helper()
	if s := h.f.leas.Sync(h.ctx()); s != engine.LeaseLeader {
		h.t.Fatalf("lease state = %v, want LeaseLeader", s)
	}
}

func (h *harness) orch()  { h.f.Run(h.ctx()) }
func (h *harness) local() { h.f.RunLocal(h.ctx()) }

func (h *harness) node(name string) {
	h.labels[name] = nil
	h.fake.SetNode(name, nil, false, nil, "uid-"+name)
}

func (h *harness) cpNode(name string) {
	l := map[string]string{"node-role.kubernetes.io/control-plane": ""}
	h.labels[name] = l
	h.fake.SetNode(name, nil, false, l, "uid-"+name)
}

// request simulates the API admission patch (fresh requested state).
func (h *harness) request(name string, force bool) {
	h.t.Helper()
	h.fake.SetNode(name, map[string]string{
		nodestate.AnnState:   nodestate.StateValue(nodestate.Requested, h.cl.Now()),
		nodestate.AnnRequest: nodestate.RequestValue("req-1", "test", force),
	}, false, h.labels[name], "uid-"+name)
}

// clear simulates DELETE /reboots/<node> (all four keys removed).
func (h *harness) clear(name string) {
	h.t.Helper()
	h.fake.SetNode(name, nil, false, h.labels[name], "uid-"+name)
}

func (h *harness) st(name string) nodestate.NodeState {
	h.t.Helper()
	n, ok := h.fake.GetNode(name)
	if !ok {
		h.t.Fatalf("node %s missing", name)
	}
	md, _ := n["metadata"].(map[string]any)
	anns := map[string]string{}
	if a, ok := md["annotations"].(map[string]any); ok {
		for k, v := range a {
			anns[k] = v.(string)
		}
	}
	return *nodestate.Parse(anns)
}

func (h *harness) stateOf(name string) nodestate.State {
	s := h.st(name)
	if s.State == nil || !s.State.Present {
		return ""
	}
	return s.State.State
}

func (h *harness) unsched(name string) bool {
	h.t.Helper()
	n, _ := h.fake.GetNode(name)
	b, _ := n["spec"].(map[string]any)["unschedulable"].(bool)
	return b
}

func (h *harness) pod(ns, name, nodeName string, owners ...string) *kubetest.Pod {
	p := &kubetest.Pod{Name: name, Namespace: ns, NodeName: nodeName, Phase: "Running", Owners: owners}
	h.fake.AddPod(p)
	return p
}

func (h *harness) podLabels(p *kubetest.Pod, l map[string]string) *kubetest.Pod {
	p.Labels = l
	return p
}

func (h *harness) pdb(ns, name string, sel map[string]string, allowed int32) *kubetest.PDB {
	p := &kubetest.PDB{Name: name, Namespace: ns, Selector: &kube.LabelSelector{MatchLabels: sel}, Allowed: allowed}
	h.fake.AddPDB(p)
	return p
}

func (h *harness) eventCount(reason string) int {
	n := 0
	for _, e := range h.fake.Events() {
		if e["reason"] == reason {
			n++
		}
	}
	return n
}

// driveToRebooting requests the node and cycles until it is rebooting.
func (h *harness) driveToRebooting(name string, force bool) {
	h.t.Helper()
	h.node(name)
	h.request(name, force)
	h.leader()
	h.orch()
	if got := h.stateOf(name); got != nodestate.Draining {
		h.t.Fatalf("state = %q, want draining", got)
	}
	h.orch()
	if got := h.stateOf(name); got != nodestate.Rebooting {
		h.t.Fatalf("state = %q, want rebooting", got)
	}
}

// issue drives the local pod to issue the reboot (exec recorded + nsenter).
func (h *harness) issue(node string) {
	h.t.Helper()
	h.local()
	s := h.st(node)
	if s.Exec == nil || !s.Exec.Present || s.Exec.Attempt != 1 || h.reboots != 1 {
		h.t.Fatalf("exec not issued: %+v reboots=%d", s.Exec, h.reboots)
	}
}

func TestHappyPath(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	if !h.unsched("w1") {
		t.Fatal("node should be cordoned while draining")
	}
	h.issue("w1")

	// Host reboots: boot ID changes, node comes back Ready.
	h.bootID = "boot-2"
	h.fake.SetNodeReady("w1", false)
	h.cl.Advance(time.Second)
	h.local() // confirm
	if got := h.st("w1").Exec.ConfirmedAt; got == nil {
		t.Fatal("confirmedAt not set after boot ID change")
	}
	h.fake.SetNodeReady("w1", true)
	h.orch() // completed
	if got := h.stateOf("w1"); got != nodestate.Completed {
		t.Fatalf("state = %q, want completed", got)
	}
	h.orch() // next cycle: uncordon
	if h.unsched("w1") {
		t.Fatal("node should be uncordoned after completion")
	}
	for _, reason := range []string{"RebootDraining", "RebootIssued", "RebootCommandIssued", "RebootCompleted"} {
		if h.eventCount(reason) == 0 {
			t.Fatalf("missing event %s", reason)
		}
	}
}

func TestPDBBlockedLaterNodesProceed(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.node("w2")
	h.podLabels(h.pod("app", "p1", "w1", "ReplicaSet"), map[string]string{"app": "web"})
	p := h.pdb("app", "web", map[string]string{"app": "web"}, 0)
	h.request("w1", false)
	h.request("w2", false)
	h.leader()
	h.orch()

	// w1 (first in queue) is PDB-blocked; w2 proceeds.
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("w1 state = %q, want requested (blocked)", got)
	}
	if b := h.st("w1").Status; b == nil || b.BlockedBy == nil || len(b.BlockedBy) != 1 || b.BlockedBy[0] != "app/web" {
		t.Fatalf("w1 blockedBy = %+v, want [app/web]", b)
	}
	if got := h.stateOf("w2"); got != nodestate.Draining {
		t.Fatalf("w2 state = %q, want draining", got)
	}

	// Fix the PDB: w1 still waits while w2 is in flight.
	p.Allowed = 1
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("w1 state = %q, want requested (slot full)", got)
	}
	// w2 finishes the drain; clear it to free the slot.
	h.orch()
	h.clear("w2")
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("w1 state = %q, want draining", got)
	}
	if b := h.st("w1").Status; b != nil && b.Present && b.BlockedBy != nil {
		t.Fatalf("w1 blockedBy should be cleared: %+v", b)
	}
}

func TestDrainTimeoutPDBDenied(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.node("w2")
	h.pod("app", "p1", "w1", "ReplicaSet")
	h.fake.EvictDeny = func(ns, pod string) bool { return true }
	h.request("w1", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining", got)
	}
	// Eviction denied (429): drain stays pending, backoff applies.
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining (denied)", got)
	}
	// Past the drain timeout: failed.
	h.cl.Advance(5 * time.Minute)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed", got)
	}
	if s := h.st("w1").Status; s == nil || s.Error == "" {
		t.Fatalf("failed node has no error: %+v", s)
	}
	// Orchestrator uncordons the failed node.
	h.orch()
	if h.unsched("w1") {
		t.Fatal("failed node should be uncordoned")
	}
	// Queue paused: a new request is not admitted while w1 is failed.
	h.request("w2", false)
	h.orch()
	if got := h.stateOf("w2"); got != nodestate.Requested {
		t.Fatalf("w2 state = %q, want requested (queue paused)", got)
	}
	if h.eventCount("QueuePaused") == 0 {
		t.Fatal("missing QueuePaused event")
	}
	// DELETE the failed node: the queue resumes.
	h.clear("w1")
	h.orch()
	if got := h.stateOf("w2"); got != nodestate.Draining {
		t.Fatalf("w2 state = %q, want draining (queue resumed)", got)
	}
}

func TestDrainTimeoutContinueOnFailure(t *testing.T) {
	h := newHarness(t, harnessOpts{onFailure: "continue"})
	h.node("w1")
	h.node("w2")
	h.pod("app", "p1", "w1", "ReplicaSet")
	h.fake.EvictDeny = func(ns, pod string) bool { return true }
	h.request("w1", false)
	h.request("w2", false)
	h.leader()
	h.orch() // w1 draining (w2 waits: slot full)
	h.cl.Advance(5 * time.Minute)
	h.orch() // w1 fails; with on-failure=continue, w2 is admitted the same cycle
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("w1 state = %q, want failed", got)
	}
	if got := h.stateOf("w2"); got != nodestate.Draining {
		t.Fatalf("w2 state = %q, want draining (on-failure=continue)", got)
	}
	h.orch()
	if got := h.stateOf("w2"); got != nodestate.Rebooting {
		t.Fatalf("w2 state = %q, want rebooting", got)
	}
}

func TestUnmanagedBlocksDrainWithoutForce(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.pod("app", "p1", "w1") // no owners
	h.request("w1", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining", got)
	}
	h.orch() // drain cycle: unmanaged pod is not evicted
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining (unmanaged blocks)", got)
	}
	h.cl.Advance(5 * time.Minute)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed (timeout)", got)
	}
}

func TestForceDeletesUnmanaged(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.pod("app", "p1", "w1") // no owners
	h.request("w1", true)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("state = %q, want draining", got)
	}
	h.orch() // eviction accepted, then force DELETE removes the pod
	h.orch() // no evictables left -> rebooting
	if got := h.stateOf("w1"); got != nodestate.Rebooting {
		t.Fatalf("state = %q, want rebooting", got)
	}
}

func TestForceDeleteErrorFails(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.pod("app", "p1", "w1") // no owners
	h.request("w1", true)
	h.leader()
	h.orch()
	h.fake.InjectFailure("pods/p1", 3) // DELETE fails
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed (force-delete error)", got)
	}
	if s := h.st("w1").Status; s == nil || s.Error == "" {
		t.Fatal("failed node has no error")
	}
}

func TestDaemonSetAndStaticPodsSkipped(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.pod("kube-system", "ds-1", "w1", "DaemonSet")
	m := h.pod("kube-system", "static-1", "w1", "Node")
	m.Annotations = map[string]string{"kubernetes.io/config.mirror": "x"}
	h.request("w1", false)
	h.leader()
	h.orch()
	h.orch() // both pods are on the skip list: drain done
	if got := h.stateOf("w1"); got != nodestate.Rebooting {
		t.Fatalf("state = %q, want rebooting", got)
	}
}

func TestNoEffectFails(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	h.issue("w1")
	// Boot ID unchanged past the issue grace: no-effect failure.
	h.cl.Advance(2*time.Minute + time.Second)
	h.local()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed (no effect)", got)
	}
	if s := h.st("w1").Status; s == nil || s.Error == "" {
		t.Fatal("no-effect failure has no error")
	}
}

func TestReissueWithinGraceThenNoEffect(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	h.issue("w1")

	// Boot ID unchanged within the grace: one re-issue (attempt 2).
	h.cl.Advance(30 * time.Second)
	h.local()
	if s := h.st("w1").Exec; s.Attempt != 2 || h.reboots != 2 {
		t.Fatalf("re-issue not applied: attempt=%d reboots=%d", s.Attempt, h.reboots)
	}
	// No second re-issue.
	h.cl.Advance(30 * time.Second)
	h.local()
	if h.reboots != 2 {
		t.Fatalf("second re-issue happened: reboots=%d", h.reboots)
	}
	// Past the grace from the re-issue: failed.
	h.cl.Advance(2 * time.Minute)
	h.local()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed", got)
	}
}

func TestCommandStartFailure(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	h.startErr = errors.New("nsenter: not found")
	h.local()
	if got := h.stateOf("w1"); got != nodestate.Failed {
		t.Fatalf("state = %q, want failed (command start error)", got)
	}
	if s := h.st("w1").Exec; s == nil || !s.Present {
		t.Fatal("exec annotation should record the attempt even when the command fails to start")
	}
}

func TestCompletedByNotReadyEvidence(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	h.issue("w1")
	// The local pod died before confirming (no more local cycles), but
	// the host rebooted: the leader observes NotReady after issuedAt.
	h.bootID = "boot-2"
	h.fake.SetNodeReady("w1", false)
	h.cl.Advance(time.Second)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Rebooting {
		t.Fatalf("state = %q, want rebooting (not ready)", got)
	}
	h.fake.SetNodeReady("w1", true)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Completed {
		t.Fatalf("state = %q, want completed (NotReady evidence)", got)
	}
}

func TestRebootingWithoutExecWaits(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	// Node is Ready but no exec: no completion evidence, it waits.
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Rebooting {
		t.Fatalf("state = %q, want rebooting (waiting for issue)", got)
	}
}

func TestCPHardLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{maxConcurrent: 2})
	h.cpNode("cp1")
	h.cpNode("cp2")
	h.request("cp1", false)
	h.request("cp2", false)
	h.leader()
	h.orch()
	if got := h.stateOf("cp1"); got != nodestate.Draining {
		t.Fatalf("cp1 state = %q, want draining", got)
	}
	if got := h.stateOf("cp2"); got != nodestate.Requested {
		t.Fatalf("cp2 state = %q, want requested (CP hard limit)", got)
	}
	// The CP hard limit blocks ALL admissions while a CP node is in-flight.
	h.node("w1")
	h.cl.Advance(time.Second) // w1 is strictly younger than cp2
	h.request("w1", false)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("w1 state = %q, want requested (CP hard limit)", got)
	}
	// Clear the CP node: the second CP node (oldest) is admitted.
	h.clear("cp1")
	h.orch()
	if got := h.stateOf("cp2"); got != nodestate.Draining {
		t.Fatalf("cp2 state = %q, want draining", got)
	}
}

func TestQueueHeldWhenRequestedNodeNotReady(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.node("w1")
	h.node("w2")
	h.request("w1", false)
	h.request("w2", false)
	h.fake.SetNodeReady("w2", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Requested {
		t.Fatalf("w1 state = %q, want requested (queue held)", got)
	}
	if h.eventCount("QueueHeldNotReady") == 0 {
		t.Fatal("missing QueueHeldNotReady event")
	}
	// w2 recovers: the queue proceeds.
	h.fake.SetNodeReady("w2", true)
	h.orch()
	if got := h.stateOf("w1"); got != nodestate.Draining {
		t.Fatalf("w1 state = %q, want draining", got)
	}
}

func TestCorruptStateHoldsSlot(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.labels["w1"] = nil
	h.fake.SetNode("w1", map[string]string{nodestate.AnnState: `{"state":`}, false, nil, "uid-w1")
	h.node("w2")
	h.request("w2", false)
	h.leader()
	h.orch()
	if got := h.stateOf("w2"); got != nodestate.Requested {
		t.Fatalf("w2 state = %q, want requested (corrupt holds slot)", got)
	}
	if h.eventCount("CorruptRebootState") != 1 {
		t.Fatalf("CorruptRebootState events = %d, want 1", h.eventCount("CorruptRebootState"))
	}
	h.orch()
	if h.eventCount("CorruptRebootState") != 1 {
		t.Fatal("CorruptRebootState event not rate-limited")
	}
	// Repair: clear the corrupt node, the queue proceeds.
	h.clear("w1")
	h.orch()
	if got := h.stateOf("w2"); got != nodestate.Draining {
		t.Fatalf("w2 state = %q, want draining", got)
	}
}

func TestNodeDisappearanceEvent(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.driveToRebooting("w1", false)
	// The node leaves the cluster mid-plan.
	h.fake.RemoveNode("w1")
	h.orch()
	if h.eventCount("NodeDisappeared") != 1 {
		t.Fatalf("NodeDisappeared events = %d, want 1", h.eventCount("NodeDisappeared"))
	}
	h.orch()
	if h.eventCount("NodeDisappeared") != 1 {
		t.Fatal("NodeDisappeared event not one-shot")
	}
}
