package update

import (
	"context"
	"errors"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// reconcileBootloader implements the change-triggered bootloader
// reconciliation (PLAN.md §3.7) plus the state re-arm riding it (§3.4
// decision 14, case 1) and the malformed-goal correction (§3.10 path
// 1). It is always ungated — it runs regardless of updates.windows
// and update-mode: it never touches the release repository, only the
// partition, and it exists to honor the operator's own edits.
//
// Mount discipline: the partition is mounted only on pod start
// (unknown applied value) or on a goal change; steady state is zero
// disk I/O (and zero API calls: the start-of-cycle node object
// suffices when nothing changed). bootstrapped reports whether
// bootstrap just anchored, in which case the start-of-cycle view is
// known-stale and a fresh read is required. It returns dirty=true when
// it wrote anything (callers re-fetch before evaluating eligibility).
func (f *Feature) reconcileBootloader(ctx context.Context, node *kube.Node, bootstrapped bool) bool {
	if f.cfg.Store == nil {
		return false
	}
	name := node.Metadata.Name
	startUI := nodestate.ParseUpdate(node.Metadata.Annotations)
	f.mu.Lock()
	known, last := f.goalKnown, f.lastGoal
	f.mu.Unlock()
	if known && !bootstrapped && startUI.NextKernel == last &&
		(startUI.NextKernelPresent || last == "") {
		return false // steady state: nothing changed
	}
	fresh, err := f.kube.GetNode(ctx, name)
	if err != nil {
		f.log.Debug("update: reconcile: own node re-read failed", "err", err)
		return false
	}
	freshUI := nodestate.ParseUpdate(fresh.Metadata.Annotations)
	freshVal := ""
	if freshUI.NextKernelPresent {
		freshVal = freshUI.NextKernel
	}
	running := RunningVersion(fresh.Status.NodeInfo.KernelVersion)
	arch, archOK := MapArch(fresh.Status.NodeInfo.Architecture)
	if freshVal == "" {
		// Absent: bootstrap owns anchoring; just record.
		f.setGoalKnown("")
		return false
	}
	if freshUI.NextKernelParseErr != "" {
		// Malformed goal: correct to the safe state (ungated, no
		// repository involved — §3.10 path 1).
		if !archOK {
			f.log.Debug("update: malformed goal but node arch unsupported; skipping correction")
			f.setGoalKnown(freshVal)
			return false
		}
		return f.applySafeState(ctx, name, freshVal, running, arch)
	}
	if !archOK {
		f.log.Debug("update: node arch unsupported; skipping bootloader reconcile")
		f.setGoalKnown(freshVal)
		return false
	}
	if err := f.cfg.Store.EnsureBootGoal(ctx, freshVal, arch); err != nil {
		if errors.Is(err, ErrGoalAbsent) {
			// Pin ahead of staging: the annotation leads, the
			// file arrives later; staging completes it (§3.4).
			f.setGoalKnown(freshVal)
			return false
		}
		f.log.Warn("update: bootloader reconcile failed; retrying next cycle", "version", freshVal, "err", err)
		return false
	}
	f.setGoalKnown(freshVal)
	// Re-arm case 1: the goal changed to a well-formed present
	// version — clear a previous attempt's completed state.
	return f.rearmIfCompleted(ctx, name)
}

func (f *Feature) setGoalKnown(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGoal, f.goalKnown = v, true
}

// rearmIfCompleted clears a completed M1 state (PLAN.md §3.4 decision
// 14). Absent is a silent no-op; failed is never cleared. It reports
// whether the patch landed.
func (f *Feature) rearmIfCompleted(ctx context.Context, nodeName string) bool {
	err := nodestate.PatchTransition(ctx, f.kube, nodeName, 3, nodestate.RearmBuild())
	if err == nil {
		f.log.Info("update: M1 state re-armed for next attempt", "node", nodeName)
		return true
	}
	if err != nodestate.ErrAbort {
		f.log.Warn("update: re-arm failed", "node", nodeName, "err", err)
	}
	return false
}

// safeState computes the safe state for an unreachable goal (PLAN.md
// §3.10): running if its file is present, else the newest local
// version, else delete-the-annotation.
func (f *Feature) safeState(ctx context.Context, running string) (target string, del bool, err error) {
	local, err := f.cfg.Store.Versions(ctx)
	if err != nil {
		return "", false, err
	}
	for _, v := range local {
		if running != "" && v == running {
			return running, false, nil
		}
	}
	newest := ""
	for _, v := range local {
		if newest == "" || NewerTS(v, newest) {
			newest = v
		}
	}
	if newest != "" {
		return newest, false, nil
	}
	return "", true, nil
}

// applySafeState corrects an unreachable goal to the safe state:
// annotation RMW (set or delete, preconditioned on the value still
// being badVal), UpdateGoalCorrected event, bootloader re-point and
// normal-rule re-arm for a set value. It reports whether the
// correction landed.
func (f *Feature) applySafeState(ctx context.Context, nodeName, badVal, running, arch string) bool {
	target, del, err := f.safeState(ctx, running)
	if err != nil {
		f.log.Debug("update: safe-state scan failed; retrying next cycle", "err", err)
		return false
	}
	err = nodestate.PatchTransition(ctx, f.kube, nodeName, 3, nodestate.CorrectGoalBuild(badVal, target))
	if err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: goal correction failed", "node", nodeName, "err", err)
		}
		return false
	}
	if del {
		f.event(nodeName, "UpdateGoalCorrected",
			"next-kernel "+badVal+" unreachable and no local kernel remains; annotation deleted", true)
		f.log.Info("update: goal deleted (empty partition safe state)", "node", nodeName, "was", badVal)
		f.setGoalKnown("")
		return true
	}
	f.event(nodeName, "UpdateGoalCorrected",
		"next-kernel "+badVal+" unreachable; corrected to "+target, true)
	f.log.Info("update: goal corrected to safe state", "node", nodeName, "was", badVal, "now", target)
	// The corrected value is present by construction; re-point (own
	// mount, same cycle) and re-arm per the normal rule.
	if err := f.cfg.Store.EnsureBootGoal(ctx, target, arch); err != nil {
		f.log.Warn("update: re-point after correction failed; retrying next cycle", "node", nodeName, "err", err)
		return true // annotation already corrected; reconcile retries the re-point
	}
	f.setGoalKnown(target)
	f.rearmIfCompleted(ctx, nodeName)
	return true
}
