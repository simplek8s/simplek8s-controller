package reboot

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// startCommand starts the reboot command without waiting for it (the
// host is about to go away). A non-nil error means the command failed
// to start (missing binary, permissions).
func startCommand(ctx context.Context, cmd []string) error {
	c := exec.CommandContext(context.WithoutCancel(ctx), cmd[0], cmd[1:]...)
	c.Stdout = nil
	c.Stderr = nil
	return c.Start()
}

// runLocal is the executor role: this pod manages the reboot of its own
// node only, and only while the node is rebooting (PLAN 3.5 step 5).
// All writes are conditional and re-read the node first; otherwise
// nothing is written.
func (f *Feature) runLocal(ctx context.Context) {
	node, err := f.kube.GetNode(ctx, f.cfg.NodeName)
	if err != nil {
		return
	}
	st := nodestate.Parse(node.Metadata.Annotations)
	if st.State == nil || st.State.ParseError != "" || st.State.State != nodestate.Rebooting {
		return
	}
	bootID, err := f.cfg.BootIDFunc()
	if err != nil {
		f.log.Warn("cannot read host boot ID; reboot checks skipped", "err", err)
		return
	}
	now := f.cfg.Now()
	execInfo := st.Exec

	switch {
	case execInfo == nil || !execInfo.Present:
		// Issue (idempotent): precondition state==rebooting and no
		// readable exec. The patch goes first; nsenter only after it
		// succeeds.
		e := nodestate.ExecInfo{
			IssuedAt:       now,
			BootID:         bootID,
			Attempt:        1,
			ExecutorPodUID: f.cfg.PodUID,
		}
		patched, err := f.guardedExecPatch(ctx, node.Metadata.ResourceVersion,
			func(freshSt *nodestate.NodeState) bool {
				return freshSt.Exec == nil || !freshSt.Exec.Present
			},
			e)
		if err != nil {
			return
		}
		if !patched {
			return
		}
		f.issueReboot()

	case execInfo.ParseError != "":
		// Corrupt exec: the checks simply do not run until fixed (3.3.3).
		f.noteEvent("corrupt-exec:"+f.cfg.NodeName, f.cfg.NodeName, "CorruptRebootExec",
			"reboot-exec annotation is unparseable; issue/confirm checks are paused until it is repaired or the node is cleared with DELETE /reboots/"+f.cfg.NodeName, true, true)

	case execInfo.BootID == bootID:
		// Boot ID unchanged: either a crash between patch and nsenter
		// (re-issue once within the grace) or a no-effect reboot
		// (fail past the grace, measured from the latest issuedAt).
		switch {
		case execInfo.Attempt == 1 && !now.After(execInfo.IssuedAt.Add(f.cfg.IssueGrace)):
			e := nodestate.ExecInfo{
				IssuedAt:       now,
				BootID:         bootID,
				Attempt:        2,
				ExecutorPodUID: f.cfg.PodUID,
			}
			patched, err := f.guardedExecPatch(ctx, node.Metadata.ResourceVersion,
				func(freshSt *nodestate.NodeState) bool {
					return freshSt.Exec.Present && freshSt.Exec.ParseError == "" &&
						freshSt.Exec.Attempt == 1 &&
						freshSt.Exec.BootID == bootID &&
						!f.cfg.Now().After(freshSt.Exec.IssuedAt.Add(f.cfg.IssueGrace))
				},
				e)
			if err != nil || !patched {
				return
			}
			f.issueReboot()
		case now.After(execInfo.IssuedAt.Add(f.cfg.IssueGrace)):
			f.localFailed(ctx, "reboot did not take effect: host boot ID unchanged "+f.cfg.IssueGrace.String()+" after issuedAt")
		}
		// attempt==2 within grace: wait; no second re-issue.

	default:
		// Boot ID changed: the reboot happened. Add confirmedAt once.
		if execInfo.ConfirmedAt == nil {
			confirmed := now
			_, err := f.guardedExecPatch(ctx, node.Metadata.ResourceVersion,
				func(freshSt *nodestate.NodeState) bool {
					return freshSt.Exec.Present && freshSt.Exec.ParseError == "" && freshSt.Exec.ConfirmedAt == nil
				},
				nodestate.ExecInfo{
					IssuedAt:       execInfo.IssuedAt,
					BootID:         execInfo.BootID,
					Attempt:        execInfo.Attempt,
					ExecutorPodUID: execInfo.ExecutorPodUID,
					ConfirmedAt:    &confirmed,
				})
			if err != nil {
				f.log.Warn("confirm patch failed", "err", err)
			}
		}
	}
}

// guardedExecPatch re-reads the node, checks the guard against the fresh
// state (and that the node is still rebooting), and patches reboot-exec
// in one conditional step. It reports whether the patch was applied.
func (f *Feature) guardedExecPatch(ctx context.Context, initialRV string, guard func(fresh *nodestate.NodeState) bool, e nodestate.ExecInfo) (bool, error) {
	var applied bool
	err := nodestate.PatchTransition(ctx, f.kube, f.cfg.NodeName, 3, func(fresh *kube.Node) (map[string]any, bool) {
		st := nodestate.Parse(fresh.Metadata.Annotations)
		if st.State == nil || st.State.ParseError != "" || st.State.State != nodestate.Rebooting {
			return nil, false
		}
		if !guard(st) {
			return nil, false
		}
		applied = true
		return nodestate.IssuedExecPatch(fresh.Metadata.ResourceVersion, e), true
	})
	if err == nodestate.ErrAbort {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return applied, nil
}

// issueReboot runs the reboot command; a start failure marks the node
// failed (state + error in one conditional patch).
func (f *Feature) issueReboot() {
	err := f.cfg.StartReboot(context.Background())
	if err == nil {
		f.event(f.cfg.NodeName, "RebootCommandIssued", "reboot command issued into the host PID namespace", false)
		return
	}
	f.log.Error("reboot command failed to start", "err", err)
	f.localFailed(context.Background(), fmt.Sprintf("reboot command failed to start: %v", err))
}

// localFailed marks the node failed (local pod's two failed paths).
// Conditional: only while the node is still rebooting; the loser of a
// race writes nothing (3.3.2).
func (f *Feature) localFailed(ctx context.Context, errMsg string) {
	now := f.cfg.Now()
	err := nodestate.PatchTransition(ctx, f.kube, f.cfg.NodeName, 3, func(fresh *kube.Node) (map[string]any, bool) {
		st := nodestate.Parse(fresh.Metadata.Annotations)
		if st.State == nil || st.State.ParseError != "" || st.State.State != nodestate.Rebooting {
			return nil, false
		}
		statusRaw, _ := fresh.Metadata.Annotations[nodestate.AnnStatus]
		return nodestate.ToFailedPatch(fresh.Metadata.ResourceVersion, now, statusRaw, errMsg), true
	})
	if err == nil {
		f.event(f.cfg.NodeName, "RebootFailed", errMsg, true)
	} else if err != nodestate.ErrAbort {
		f.log.Warn("local failed patch failed", "err", err)
	}
}
