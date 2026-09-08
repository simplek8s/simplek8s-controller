package update

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// pnode is one node's parsed view for the plan cycle.
type pnode struct {
	name    string
	st      *nodestate.NodeState
	ui      *nodestate.UpdateInfo
	running string
}

// rebootState returns the node's clean reboot lifecycle state, or
// ok=false when the state is absent or corrupt (callers treat that as
// "not in a known state").
func rebootState(st *nodestate.NodeState) (nodestate.State, bool) {
	if st.State == nil || !st.State.Present || st.State.ParseError != "" {
		return "", false
	}
	return st.State.State, true
}

// Run implements engine.OrchestratorTask (leader only, PLAN-M2 3.8/3.10).
// Each cycle it: (1) starts a plan from fresh full-mode stages when none
// is active; (2) manages the active plan (admit members, verify, cancel,
// two-phase reset); (3) runs the non-plan verification; and (4)
// idempotently clears settled plan triggers. All writes are conditional
// RMWs; the plan ConfigMap is written only on transitions.
func (f *Feature) Run(ctx context.Context) {
	nodes, err := f.kube.ListNodes(ctx)
	if err != nil {
		return
	}
	plans, err := f.plans.load(ctx)
	if err != nil {
		f.log.Warn("update: plan load failed; skipping plan cycle", "err", err)
		return
	}
	now := f.cfg.Now()
	views := make(map[string]*pnode, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		views[n.Metadata.Name] = &pnode{
			name:    n.Metadata.Name,
			st:      nodestate.Parse(n.Metadata.Annotations),
			ui:      nodestate.ParseUpdate(n.Metadata.Annotations),
			running: RunningVersion(n.Status.NodeInfo.KernelVersion),
		}
	}

	if len(plans) == 0 {
		f.maybeStartPlan(ctx, views, now, plans)
	}
	for v, entry := range plans {
		f.managePlan(ctx, v, entry, plans, views, now)
	}
	f.nonPlanVerify(ctx, views, plans)
	f.clearSettled(ctx, views)
}

// maybeStartPlan starts a plan from the fresh full-mode stages present on
// nodes (the reboot-eligible trigger), when no plan is active. Membership
// is the set of non-quiescent nodes carrying the (newest) eligible
// version; a node already quiescent on its target is a stale trigger and
// is skipped (clearSettled removes it). One plan at a time: the newest
// version is planned, the rest wait for the next cycle.
func (f *Feature) maybeStartPlan(ctx context.Context, views map[string]*pnode, now time.Time, plans map[string]PlanEntry) {
	byVersion := map[string][]string{}
	for _, pn := range views {
		if !pn.ui.RebootEligiblePresent || pn.ui.RebootEligibleParseErr != "" {
			continue
		}
		if isQuiescent(pn) {
			continue // stale trigger: already on its target
		}
		byVersion[pn.ui.RebootEligible] = append(byVersion[pn.ui.RebootEligible], pn.name)
	}
	if len(byVersion) == 0 {
		return
	}
	v := ""
	for cand := range byVersion {
		if v == "" || NewerTS(cand, v) {
			v = cand
		}
	}
	members := byVersion[v]
	sort.Strings(members)
	entry := PlanEntry{
		StartedAt: now.UTC().Format(time.RFC3339),
		Nodes:     members,
	}
	plans[v] = entry
	if err := f.plans.save(ctx, plans); err != nil {
		f.log.Warn("update: failed to persist new plan", "version", v, "err", err)
		return
	}
	f.resetStaleRebootState(ctx, members, views)
	f.event("", "UpdatePlanStarted",
		fmt.Sprintf("update plan %s started for %d node(s): %s", v, len(members), strings.Join(members, ", ")), false)
	f.log.Info("update: plan started", "version", v, "members", len(members))
}

// resetStaleRebootState (BUG 12) resets each new plan member's stale terminal
// M1 reboot state (completed or failed) to the resting state before the plan's
// first verify. A stale completed would otherwise be read by managePlan's
// verify as "came up on the wrong kernel" and cancel the plan in the very
// cycle it is created, before the plan has a chance to enqueue the member.
// Each reset is a conditional RMW (ClearStaleRebootStateBuild only touches a
// present, uncorrupt terminal state — an absent, queued, or in-flight node is
// left alone) guarded by an ownership check, and the in-memory view is updated
// so this same cycle's verify (which reads the snapshot, not a re-read) sees
// the reset state.
func (f *Feature) resetStaleRebootState(ctx context.Context, members []string, views map[string]*pnode) {
	for _, m := range members {
		pn := views[m]
		if pn == nil {
			continue
		}
		if !f.leas.VerifyOwnership(ctx) {
			return
		}
		err := nodestate.PatchTransition(ctx, f.kube, m, 3, nodestate.ClearStaleRebootStateBuild())
		switch {
		case err == nil:
			pn.st.State = &nodestate.StateInfo{} // reflect the reset in this cycle's view
			f.log.Info("update: reset member's stale reboot-state before plan", "node", m)
		case err == nodestate.ErrAbort:
			// not a stale terminal state (absent/queued/in-flight): nothing to reset
		default:
			f.log.Warn("update: failed to reset member's stale reboot-state", "node", m, "err", err)
		}
	}
}

// managePlan drives one plan for the cycle: verify (may cancel), admit
// not-yet-queued members (when not canceling), apply the two-phase reset
// (when canceling), and clear the entry once every member has settled.
func (f *Feature) managePlan(ctx context.Context, v string, entry PlanEntry, plans map[string]PlanEntry, views map[string]*pnode, now time.Time) {
	if !entry.Canceling {
		if cancel, reason, node := f.verifyPlan(v, entry, views); cancel {
			entry.Canceling = true
			plans[v] = entry
			if err := f.plans.save(ctx, plans); err != nil {
				f.log.Warn("update: failed to persist cancel", "version", v, "err", err)
			}
			f.event(node, "UpdatePlanCanceled",
				fmt.Sprintf("update plan %s cancelled: %s; in-flight members settle when their reboot lands", v, reason), true)
		}
	}

	if !entry.Canceling {
		for _, m := range entry.Nodes {
			pn := views[m]
			if pn == nil || pn.st.InLifecycle() {
				continue // gone, or already queued/draining/rebooting/settled
			}
			f.admitMember(ctx, v, m, now)
		}
	}

	if entry.Canceling {
		f.resetMembers(ctx, v, entry, views)
	}

	if f.allSettled(entry, views) {
		delete(plans, v)
		if err := f.plans.save(ctx, plans); err != nil {
			f.log.Warn("update: failed to persist plan clear", "version", v, "err", err)
			return
		}
		f.log.Info("update: plan settled and cleared", "version", v, "members", len(entry.Nodes), "canceled", entry.Canceling)
	}
}

// verifyPlan reports whether the plan must be cancelled: a member that
// failed its reboot, or one that came up on a kernel other than the plan
// version (verification mismatch, PLAN-M2 3.10).
func (f *Feature) verifyPlan(v string, entry PlanEntry, views map[string]*pnode) (bool, string, string) {
	for _, m := range entry.Nodes {
		pn := views[m]
		if pn == nil {
			continue
		}
		st, ok := rebootState(pn.st)
		if !ok {
			continue
		}
		switch st {
		case nodestate.Failed:
			return true, fmt.Sprintf("member %s failed its reboot", m), m
		case nodestate.Completed:
			if pn.running != v {
				return true, fmt.Sprintf("member %s came up on %s, not %s", m, pn.running, v), m
			}
		}
	}
	return false, "", ""
}

// admitMember enqueues a plan member into the M1 reboot queue
// (reboot-state := requested, no request → PDB-gated, not forced). The M1
// orchestrator then drains/reboots/confirms it like any queued reboot.
func (f *Feature) admitMember(ctx context.Context, v, m string, now time.Time) {
	if !f.leas.VerifyOwnership(ctx) {
		return
	}
	err := nodestate.PatchTransition(ctx, f.kube, m, 3, nodestate.EnqueueBuild(now))
	switch {
	case err == nil:
		f.log.Info("update: plan member enqueued", "version", v, "node", m)
	case err != nodestate.ErrAbort:
		f.log.Warn("update: enqueue failed", "version", v, "node", m, "err", err)
	}
}

// resetMembers applies the cancel's two-phase reset (PLAN-M2 3.8):
// members that have not physically rebooted (absent/requested/drain state)
// are reset to their running kernel now; members in flight (draining /
// rebooting / corrupt) settle when their M1 state lands (a landed member
// consistent on the plan version is kept, a mismatch or failure is reset).
// Every reset is a conditional RMW whose precondition is that next-kernel
// still equals the plan version — an operator re-pin is skipped, never
// clobbered.
func (f *Feature) resetMembers(ctx context.Context, v string, entry PlanEntry, views map[string]*pnode) {
	for _, m := range entry.Nodes {
		pn := views[m]
		if pn == nil {
			continue
		}
		if pn.st.InFlight() {
			continue // deferred: settles when its M1 state lands
		}
		st, ok := rebootState(pn.st)
		switch {
		case ok && st == nodestate.Completed:
			if pn.running == v {
				continue // consistent: an updated member, keep it
			}
			f.resetToRunning(ctx, m, pn.running, v)
		case ok && st == nodestate.Failed:
			f.resetToRunning(ctx, m, pn.running, v)
		default:
			// absent or requested: not yet physically rebooting.
			if pn.running == v {
				continue // already on the plan version
			}
			f.resetToRunning(ctx, m, pn.running, v)
		}
	}
}

// resetToRunning pins next-kernel back to the node's running kernel (a
// known-good quiescent value), conditioned on it still being v.
func (f *Feature) resetToRunning(ctx context.Context, m, running, v string) {
	if running == "" {
		f.log.Warn("update: cannot reset member; running version unknown", "node", m)
		return
	}
	err := nodestate.PatchTransition(ctx, f.kube, m, 3,
		nodestate.NextKernelBuild(running, nodestate.PrecondValue(v)))
	switch {
	case err == nil:
		f.log.Info("update: plan member reset", "node", m, "from", v, "to", running)
	case err != nodestate.ErrAbort:
		f.log.Warn("update: reset failed", "node", m, "err", err)
	}
}

// allSettled reports whether every plan member is quiescent (next-kernel
// present and == running). A missing node counts as settled (it is gone;
// nothing left to converge on).
func (f *Feature) allSettled(entry PlanEntry, views map[string]*pnode) bool {
	for _, m := range entry.Nodes {
		pn := views[m]
		if pn == nil {
			continue
		}
		if !isQuiescent(pn) {
			return false
		}
	}
	return true
}

// isQuiescent reports next-kernel present, valid, and equal to running.
func isQuiescent(pn *pnode) bool {
	return pn.ui.NextKernelPresent && pn.ui.NextKernelParseErr == "" && pn.ui.NextKernel == pn.running
}

// nonPlanVerify (PLAN-M2 3.10): a node NOT in the active plan that reached
// M1 completed on a kernel different from its next-kernel fell back after a
// failed boot → single-node reset to the running kernel. A failed reboot is
// left alone (the node never left its old kernel; the operator retries).
func (f *Feature) nonPlanVerify(ctx context.Context, views map[string]*pnode, plans map[string]PlanEntry) {
	inPlan := map[string]bool{}
	for _, e := range plans {
		for _, m := range e.Nodes {
			inPlan[m] = true
		}
	}
	for name, pn := range views {
		if inPlan[name] {
			continue
		}
		st, ok := rebootState(pn.st)
		if !ok || st != nodestate.Completed {
			continue
		}
		if !pn.ui.NextKernelPresent || pn.ui.NextKernelParseErr != "" || pn.ui.NextKernel == pn.running {
			continue
		}
		if pn.running == "" {
			f.log.Warn("update: non-plan verify: running version unknown", "node", name)
			continue
		}
		err := nodestate.PatchTransition(ctx, f.kube, name, 3,
			nodestate.NextKernelBuild(pn.running, nodestate.PrecondValue(pn.ui.NextKernel)))
		switch {
		case err == nil:
			f.event(name, "UpdateNodeFailed",
				fmt.Sprintf("reboot of %s did not come up on %s (running %s); next-kernel reset to %s", name, pn.ui.NextKernel, pn.running, pn.running), true)
			f.log.Warn("update: non-plan verify reset", "node", name, "from", pn.ui.NextKernel, "to", pn.running)
		case err != nodestate.ErrAbort:
			f.log.Warn("update: non-plan reset failed", "node", name, "err", err)
		}
	}
}

// clearSettled idempotently deletes the reboot-eligible trigger on every
// quiescent node (the leader is the only writer that clears it,
// PLAN-M2 3.8). A node still pending its reboot (next-kernel != running)
// is never cleared.
func (f *Feature) clearSettled(ctx context.Context, views map[string]*pnode) {
	for name, pn := range views {
		if !pn.ui.RebootEligiblePresent || !isQuiescent(pn) {
			continue
		}
		if !f.leas.VerifyOwnership(ctx) {
			return
		}
		err := nodestate.PatchTransition(ctx, f.kube, name, 3,
			nodestate.ClearRebootEligibleBuild(pn.running))
		if err != nil && err != nodestate.ErrAbort {
			f.log.Warn("update: clear eligible failed", "node", name, "err", err)
		}
	}
}
