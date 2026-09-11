package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/config"
	"github.com/simplek8s/simplek8s-controller/internal/cron"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// maybeEnqueue enqueues the local pod's own node into the M1 reboot
// queue when it is reboot-eligible (PLAN.md §3.4, decision 31). The
// order is the ordering invariant, one code path: verify the goal
// file is present (EnsureBootGoal also re-points DEFAULT when needed)
// and only then the M1-state RMW. Precondition failure on the fresh
// read (value changed, state changed, operator edit) → skip, no
// clobbering.
func (f *Feature) maybeEnqueue(ctx context.Context, node *kube.Node, fc config.Config, now time.Time) {
	name := node.Metadata.Name
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	running := RunningVersion(node.Status.NodeInfo.KernelVersion)
	st := nodestate.Parse(node.Metadata.Annotations)

	// Rules 1–4 (rule 5, file presence, is verified just below via the
	// store — the only observer that can see the partition).
	eligible := fc.UpdateMode == "full" &&
		ui.NextKernelPresent && ui.NextKernelParseErr == "" &&
		running != "" && ui.NextKernel != running &&
		(st.State == nil || !st.State.Present)
	if !eligible {
		f.noteHeld(name, false)
		return
	}
	arch, ok := MapArch(node.Status.NodeInfo.Architecture)
	if !ok {
		f.log.Debug("update: node arch unsupported; not enqueueing")
		f.noteHeld(name, false)
		return
	}
	if f.cfg.Store == nil {
		return
	}
	if !cron.WindowsOpen(fc.RebootWindows, fc.RebootWindowGrace, now) {
		// Eligible but the reboots window is closed: wait before the
		// queue (rules 1–4 hold; file presence is re-verified at
		// enqueue time, so no wasted reboot is possible).
		f.noteHeld(name, true)
		return
	}
	f.noteHeld(name, false)

	goal := ui.NextKernel
	if err := f.cfg.Store.EnsureBootGoal(ctx, goal, arch); err != nil {
		if !errors.Is(err, ErrGoalAbsent) {
			f.log.Warn("update: boot goal ensure failed; not enqueueing", "version", goal, "err", err)
		}
		return
	}
	reqID, err := newControllerRequestID(now)
	if err != nil {
		f.log.Warn("update: request id", "err", err)
		return
	}
	err = nodestate.PatchTransition(ctx, f.kube, name, 3,
		nodestate.EnqueueControllerBuild(now, reqID, goal, running))
	switch {
	case err == nil:
		f.log.Info("update: enqueued node for reboot", "node", name, "version", goal)
	case err != nodestate.ErrAbort:
		f.log.Warn("update: enqueue failed", "node", name, "err", err)
	}
}

// noteHeld fires UpdateHeldWindow on the false->true edge (the node is
// reboot-eligible and waiting for a reboots.windows window) and clears
// the edge when the node is no longer waiting.
func (f *Feature) noteHeld(nodeName string, active bool) {
	key := "heldwindow:" + nodeName
	f.mu.Lock()
	defer f.mu.Unlock()
	if active && !f.notified[key] {
		f.notified[key] = true
		f.event(nodeName, "UpdateHeldWindow",
			"node is reboot-eligible and waiting for a reboots.windows window", false)
	}
	if !active {
		delete(f.notified, key)
	}
}

// newControllerRequestID builds "<RFC3339 UTC>-<6 random hex>" for
// pod-issued queue entries (requestedBy "controller").
func newControllerRequestID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format(time.RFC3339) + "-" + hex.EncodeToString(b[:]), nil
}
