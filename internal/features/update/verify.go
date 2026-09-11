package update

import (
	"fmt"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// verifyNodes is the leader-side per-node verification (PLAN.md §3.4,
// replacing the plan verification). Every cycle, for each node with a
// well-formed next-kernel. Always on — purely observational (no
// network, no disk): it runs even with updates.windows [] and with
// update-mode off (events only — eligibility still bars any reboot).
//
//   - running == next-kernel → quiescent; nothing to do. The transition
//     into quiescence emits UpdateApplied (keyed per node+version,
//     idempotent: one fire per version).
//   - running != next-kernel and the M1 state is completed →
//     UpdateMismatch event + warn log (keyed per node, rate-limited).
//     No automatic retry: the operator reboots via the M1 API or
//     re-pins.
func (f *Feature) verifyNodes(views map[string]*pnode) {
	seen := make(map[string]bool, len(views))
	for name, pn := range views {
		seen[name] = true
		if !pn.ui.NextKernelPresent || pn.ui.NextKernelParseErr != "" || pn.running == "" {
			continue
		}
		if pn.ui.NextKernel == pn.running {
			f.mu.Lock()
			prev, ok := f.nonQuiescent[name]
			if ok {
				delete(f.nonQuiescent, name)
			}
			f.mu.Unlock()
			if ok && prev == pn.ui.NextKernel {
				f.event(name, "UpdateApplied",
					fmt.Sprintf("node quiescent on %s after reboot", pn.running), false)
				f.log.Info("update: applied", "node", name, "version", pn.running)
			}
			f.noteMismatch(name, "", "", false)
			continue
		}
		f.mu.Lock()
		f.nonQuiescent[name] = pn.ui.NextKernel
		f.mu.Unlock()
		st, ok := rebootState(pn.st)
		f.noteMismatch(name, pn.ui.NextKernel, pn.running, ok && st == nodestate.Completed)
	}
	// Drop memory for gone nodes.
	f.mu.Lock()
	for name := range f.nonQuiescent {
		if !seen[name] {
			delete(f.nonQuiescent, name)
		}
	}
	f.mu.Unlock()
}

// noteMismatch fires UpdateMismatch on the false->true edge of the
// completed-but-diverged condition and clears the edge otherwise, so a
// re-entry after operator action re-fires exactly once.
func (f *Feature) noteMismatch(nodeName, goal, running string, active bool) {
	key := "mismatch:" + nodeName
	f.mu.Lock()
	defer f.mu.Unlock()
	if active && !f.notified[key] {
		f.notified[key] = true
		f.event(nodeName, "UpdateMismatch",
			fmt.Sprintf("node came back on %s, not %s; no auto-retry (reboot via the M1 API or re-pin)", running, goal), true)
		f.log.Warn("update: verification mismatch; no auto-retry", "node", nodeName, "running", running, "goal", goal)
	}
	if !active {
		delete(f.notified, key)
	}
}
