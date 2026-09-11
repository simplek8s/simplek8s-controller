package reboot

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/config"
	"github.com/simplek8s/simplek8s-controller/internal/cron"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// view is one node plus its parsed reboot state.
type view struct {
	node  *kube.Node
	st    *nodestate.NodeState
	ready bool
	cp    bool
}

// runOrchestrator is the leader-only cycle (PLAN 3.4/3.5).
func (f *Feature) runOrchestrator(ctx context.Context) {
	nodes, err := f.kube.ListNodes(ctx)
	if err != nil {
		return // engine already rate-limits the unreachable log
	}
	pods, err := f.kube.ListPods(ctx)
	if err != nil {
		f.log.Warn("pod list failed", "err", err)
		return
	}
	pdbs, err := f.kube.ListPDBs(ctx)
	if err != nil {
		f.log.Warn("pdb list failed", "err", err)
		return
	}
	now := f.cfg.Now()
	fc := f.cfg.Features()

	views := buildViews(nodes)
	f.observeDisappearances(views)

	// Phase 1: manage existing lifecycles (timeouts, drain, completion,
	// uncordon, corrupt-state handling).
	for _, v := range views {
		f.manageNode(ctx, v, pods, now, fc)
	}

	// Phase 2: admission (queue), from a fresh node list: phase 1 may
	// have transitioned nodes (freed a slot, recorded a failure).
	freshNodes, err := f.kube.ListNodes(ctx)
	if err != nil {
		return
	}
	inFlight, cpInFlight, anyFailed, queuedNotReady := admissionCounts(buildViews(freshNodes))
	if fc.OnRebootFailure == "pause" && anyFailed {
		f.noteEvent("queue-paused", "", "QueuePaused",
			"reboot queue paused: a node is in failed state; clear it with DELETE /reboots/<node> to resume", true, true)
		return
	}
	if queuedNotReady {
		f.noteEvent("queue-held", "", "QueueHeldNotReady",
			"reboot queue held: a requested (queued) node is NotReady", true, true)
		return
	}
	if inFlight >= fc.MaxConcurrentReboots || cpInFlight >= 1 {
		return
	}

	queue := make([]*view, 0)
	for i := range freshNodes {
		n := &freshNodes[i]
		st := nodestate.Parse(n.Metadata.Annotations)
		if st.State != nil && st.State.ParseError == "" && st.State.State == nodestate.Requested {
			queue = append(queue, &view{node: n, st: st, ready: n.Ready(), cp: n.IsControlPlane()})
		}
	}
	sortQueue(queue)

	// Window gate (PLAN.md §3.5): a non-forced candidate is admitted
	// only while a reboot window is open. now was sampled once for the
	// cycle, so every candidate sees the same openness.
	windowOpen := cron.WindowsOpen(fc.RebootWindows, fc.RebootWindowGrace, now)
	for _, v := range queue {
		if inFlight >= fc.MaxConcurrentReboots || cpInFlight >= 1 {
			break
		}
		if !forceOf(v.st) && !windowOpen {
			// Held nodes wait; the loop still serves forced
			// candidates queued behind them (continue, not break:
			// forced is the escape hatch and must not stall
			// behind a held queue).
			f.noteEvent("window:"+v.node.Metadata.Name, v.node.Metadata.Name, "QueueHeldWindow",
				"reboot waiting for a reboots.windows window; configure one or re-issue with force", false, true)
			continue
		}
		if !forceOf(v.st) {
			blocked := pdbBlocked(v.node, pdbs, pods)
			if len(blocked) > 0 {
				f.writeBlockedBy(ctx, v, blocked)
				f.noteEvent("pdb:"+v.node.Metadata.Name, v.node.Metadata.Name, "PDBBlocked",
					"reboot blocked by PDBs: "+join(blocked)+"; fix the workload/PDB, DELETE the request, or re-issue with force", true, true)
				continue // blocked node skipped; later nodes may proceed
			}
		}
		// Lease re-validated before every transition patch (3.1).
		if !f.leas.VerifyOwnership(ctx) {
			f.log.Warn("lease ownership lost before transition; skipping admission")
			return
		}
		name := v.node.Metadata.Name
		err := nodestate.PatchTransition(ctx, f.kube, name, 3, func(fresh *kube.Node) (map[string]any, bool) {
			st := nodestate.Parse(fresh.Metadata.Annotations)
			if st.State == nil || st.State.ParseError != "" || st.State.State != nodestate.Requested {
				return nil, false
			}
			statusRaw, _ := fresh.Metadata.Annotations[nodestate.AnnStatus]
			return nodestate.ToDrainingPatch(fresh.Metadata.ResourceVersion, now, statusRaw, fresh.Spec.Unschedulable), true
		})
		switch {
		case err == nil:
			inFlight++
			if v.cp {
				cpInFlight++
			}
			f.event(name, "RebootDraining", "drain started (cordon + eviction)", false)
			return // one admission per cycle
		case err == nodestate.ErrAbort:
			continue // raced into another state; try the next candidate
		default:
			f.log.Warn("admission patch failed", "node", name, "err", err)
			return
		}
	}
}

func buildViews(nodes []kube.Node) []*view {
	views := make([]*view, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		views = append(views, &view{
			node:  n,
			st:    nodestate.Parse(n.Metadata.Annotations),
			ready: n.Ready(),
			cp:    n.IsControlPlane(),
		})
	}
	return views
}

// admissionCounts derives the admission gates from the views: in-flight
// slot count (incl. corrupt), CP in-flight, any failed (pause), and a
// queued NotReady (hold).
func admissionCounts(views []*view) (inFlight, cpInFlight int, anyFailed, queuedNotReady bool) {
	for _, v := range views {
		if v.st.InFlight() {
			inFlight++
			if v.cp {
				cpInFlight++
			}
		}
		if v.st.State != nil && v.st.State.ParseError == "" {
			switch v.st.State.State {
			case nodestate.Failed:
				anyFailed = true
			case nodestate.Requested:
				if !v.ready {
					queuedNotReady = true
				}
			}
		}
	}
	return
}

// sortQueue: oldest .since first, workers before control planes, then
// node name (3.3.1).
func sortQueue(queue []*view) {
	sort.SliceStable(queue, func(i, j int) bool {
		a, b := queue[i], queue[j]
		if !a.st.State.Since.Equal(b.st.State.Since) {
			return a.st.State.Since.Before(b.st.State.Since)
		}
		if a.cp != b.cp {
			return !a.cp // workers first
		}
		return a.node.Metadata.Name < b.node.Metadata.Name
	})
}

// manageNode runs the per-node lifecycle rules for one cycle.
func (f *Feature) manageNode(ctx context.Context, v *view, pods []kube.Pod, now time.Time, fc config.Config) {
	name := v.node.Metadata.Name
	if v.st.State == nil || !v.st.State.Present {
		return
	}
	if v.st.State.ParseError != "" {
		// Corrupt reboot-state: holds a slot indefinitely, loud Event
		// with both escapes (3.3.3).
		f.noteEvent("corrupt:"+name, name, "CorruptRebootState",
			"reboot-state annotation is unparseable; the node holds a concurrency slot until the operator repairs the annotation or clears it with DELETE /reboots/"+name+
				". raw: "+v.st.State.Raw, true, true)
		return
	}
	switch v.st.State.State {
	case nodestate.Draining:
		f.manageDraining(ctx, v, pods, now, fc.RebootDrainTimeout)
	case nodestate.Rebooting:
		f.manageRebooting(ctx, v, now)
	case nodestate.Completed, nodestate.Failed:
		f.uncordonRule(ctx, v)
	}
}

// manageDraining: timeout check, then one drain cycle, then transitions.
func (f *Feature) manageDraining(ctx context.Context, v *view, pods []kube.Pod, now time.Time, drainTimeout time.Duration) {
	res, errMsg := f.runDrain(ctx, v.node, v.st, pods, drainTimeout)
	switch res {
	case drainDone:
		f.transition(ctx, v, nodestate.Draining, func(fresh *kube.Node) map[string]any {
			return nodestate.ToRebootingPatch(fresh.Metadata.ResourceVersion, now)
		}, "RebootIssued", "drain finished; node is rebooting", false)
	case drainFailed:
		now := f.cfg.Now()
		f.transition(ctx, v, nodestate.Draining, func(fresh *kube.Node) map[string]any {
			statusRaw, _ := fresh.Metadata.Annotations[nodestate.AnnStatus]
			return nodestate.ToFailedPatch(fresh.Metadata.ResourceVersion, now, statusRaw, errMsg)
		}, "RebootFailed", errMsg, true)
	}
}

// manageRebooting: NotReady observation + the completed evidence rule.
// A rebooting node without reboot-exec has no completion evidence and
// simply waits for the local pod to issue (3.3.1).
func (f *Feature) manageRebooting(ctx context.Context, v *view, now time.Time) {
	exec := v.st.Exec
	if exec == nil || !exec.Present || exec.ParseError != "" {
		return
	}
	if !v.ready && now.After(exec.IssuedAt) {
		f.mu.Lock()
		f.notReadyAfter[v.node.Metadata.Name] = now
		f.mu.Unlock()
	}
	if !v.ready {
		return
	}
	var obs time.Time
	f.mu.Lock()
	obs = f.notReadyAfter[v.node.Metadata.Name]
	f.mu.Unlock()
	evidence := exec.ConfirmedAt != nil || (!obs.IsZero() && obs.After(exec.IssuedAt))
	if !evidence {
		return
	}
	f.transition(ctx, v, nodestate.Rebooting, func(fresh *kube.Node) map[string]any {
		return nodestate.ToCompletedPatch(fresh.Metadata.ResourceVersion, now)
	}, "RebootCompleted", "node is Ready again and the reboot is confirmed", false)
}

// transition performs a conditional state transition from source state
// `from` (re-read + precondition re-evaluation on 409) and emits the
// transition Event.
func (f *Feature) transition(ctx context.Context, v *view, from nodestate.State, build func(fresh *kube.Node) map[string]any, reason, msg string, warning bool) {
	name := v.node.Metadata.Name
	err := nodestate.PatchTransition(ctx, f.kube, name, 3, func(fresh *kube.Node) (map[string]any, bool) {
		st := nodestate.Parse(fresh.Metadata.Annotations)
		if st.State == nil || st.State.ParseError != "" || st.State.State != from {
			return nil, false
		}
		return build(fresh), true
	})
	if err == nil {
		f.event(name, reason, msg, warning)
	} else if err != nodestate.ErrAbort {
		f.log.Warn("transition failed", "node", name, "err", err)
	}
}

// uncordonRule is the orchestrator's idempotent uncordon on
// failed/completed (3.3.2): uncordon only nodes the controller cordoned
// (cordonedPrev known absent); preserve the cordon (with a rate-limited
// Event) when cordonedPrev is unreadable.
func (f *Feature) uncordonRule(ctx context.Context, v *view) {
	name := v.node.Metadata.Name
	if !v.node.Spec.Unschedulable {
		return
	}
	status := v.st.Status
	switch {
	case status == nil || !status.Present:
		// cordonedPrev known absent: the controller cordoned it
		f.uncordon(ctx, v)
	case status.ParseError != "":
		f.noteEvent("uncordon-blocked:"+name, name, "UncordonBlocked",
			"reboot-status.cordonedPrev is unreadable; the cordon is preserved; explicit operator repair is required (kubectl uncordon or repair the annotation)", true, true)
	case status.CordonedPrev != nil && *status.CordonedPrev:
		// the operator cordoned it; leave it
	default:
		f.uncordon(ctx, v)
	}
}

// uncordon issues the idempotent spec.unschedulable=false patch,
// re-reading on 409 (never inside a transition patch).
func (f *Feature) uncordon(ctx context.Context, v *view) {
	name := v.node.Metadata.Name
	for attempt := 0; attempt < 3; attempt++ {
		fresh, err := f.kube.GetNode(ctx, name)
		if err != nil {
			return
		}
		if !fresh.Spec.Unschedulable {
			return
		}
		err = f.kube.PatchNode(ctx, name, nodestate.UncordonPatch(fresh.Metadata.ResourceVersion))
		if err == nil || kube.IsNotFound(err) {
			return
		}
		if !kube.IsConflict(err) {
			f.log.Warn("uncordon patch failed", "node", name, "err", err)
			return
		}
	}
}

// writeBlockedBy updates reboot-status.blockedBy via the RMW discipline
// (only while requested and blocked; each cycle).
func (f *Feature) writeBlockedBy(ctx context.Context, v *view, blocked []string) {
	name := v.node.Metadata.Name
	err := nodestate.PatchTransition(ctx, f.kube, name, 3, func(fresh *kube.Node) (map[string]any, bool) {
		st := nodestate.Parse(fresh.Metadata.Annotations)
		if st.State == nil || st.State.ParseError != "" || st.State.State != nodestate.Requested {
			return nil, false
		}
		statusRaw, _ := fresh.Metadata.Annotations[nodestate.AnnStatus]
		return nodestate.StatusPatch(fresh.Metadata.ResourceVersion, statusRaw, func(s *nodestate.StatusInfo) {
			s.BlockedBy = blocked
		}), true
	})
	if err != nil && err != nodestate.ErrAbort {
		f.log.Warn("blockedBy update failed", "node", name, "err", err)
	}
}

// observeDisappearances logs + Events nodes that left the cluster while
// in-flight (3.4: never an error, never a hold; state is annotation-
// derived so no per-node memory survives for gone nodes).
func (f *Feature) observeDisappearances(views []*view) {
	alive := map[string]nodestate.State{}
	for _, v := range views {
		if v.st.State != nil && v.st.State.Present && v.st.State.ParseError == "" {
			alive[v.node.Metadata.Name] = v.st.State.State
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, st := range f.prevInflight {
		if _, ok := alive[name]; !ok && (st == nodestate.Draining || st == nodestate.Rebooting || st == nodestate.Failed || st == nodestate.Completed) {
			f.log.Info("node disappeared mid-plan", "node", name, "state", string(st))
			f.event(name, "NodeDisappeared",
				fmt.Sprintf("node disappeared while in state %s; its reboot state was removed with it", st), false)
			delete(f.prevInflight, name)
		}
	}
	f.prevInflight = map[string]nodestate.State{}
	for name, st := range alive {
		if st == nodestate.Draining || st == nodestate.Rebooting {
			f.prevInflight[name] = st
		}
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
