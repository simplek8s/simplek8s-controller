// Package nodestate implements the four-annotation reboot state model on
// Node objects (PLAN 3.3): parse (tolerant of corruption), serialize,
// the RMW discipline for the shared reboot-status annotation, and the
// conditional-transition patch discipline (3.3.2).
package nodestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// Annotation keys (PLAN 3.3). A node participates in the reboot lifecycle
// iff reboot-state is present.
const (
	AnnState   = "simplek8s.org/reboot-state"
	AnnRequest = "simplek8s.org/reboot-request"
	AnnExec    = "simplek8s.org/reboot-exec"
	AnnStatus  = "simplek8s.org/reboot-status"
)

// State is a reboot lifecycle state.
type State string

const (
	Requested State = "requested"
	Draining  State = "draining"
	Rebooting State = "rebooting"
	Completed State = "completed"
	Failed    State = "failed"
)

// StateInfo is the parsed reboot-state annotation.
type StateInfo struct {
	Present    bool
	State      State
	Since      time.Time
	Raw        string
	ParseError string // non-empty when present but corrupt (3.3.3)
}

// RequestInfo is the parsed reboot-request annotation.
type RequestInfo struct {
	Present    bool
	ID         string
	By         string // omitted when absent
	Force      bool
	ParseError string
}

// ExecInfo is the parsed reboot-exec annotation (local-pod owned).
type ExecInfo struct {
	Present        bool
	IssuedAt       time.Time
	BootID         string
	Attempt        int
	ExecutorPodUID string
	ConfirmedAt    *time.Time
	ParseError     string
}

// StatusInfo is the parsed reboot-status annotation (shared value, RMW).
type StatusInfo struct {
	Present      bool
	ParseError   string // non-empty => whole annotation unreadable
	BlockedBy    []string
	Error        string
	CordonedPrev *bool // nil = field absent
}

// NodeState is the parsed view of a node's reboot annotations.
type NodeState struct {
	State   *StateInfo
	Request *RequestInfo
	Exec    *ExecInfo
	Status  *StatusInfo
}

// InLifecycle reports whether the node participates (reboot-state present,
// corrupt or not).
func (s *NodeState) InLifecycle() bool {
	return s.State != nil && s.State.Present
}

// InFlight reports whether the node holds a concurrency slot:
// draining/rebooting, or a corrupt reboot-state (3.3.3: conservative).
func (s *NodeState) InFlight() bool {
	if s.State == nil {
		return false
	}
	if s.State.ParseError != "" {
		return true
	}
	return s.State.State == Draining || s.State.State == Rebooting
}

// Parse decodes the four annotations tolerantly: a corrupt annotation never
// panics; its fields render as absent and ParseError is set (3.3.3).
func Parse(annotations map[string]string) *NodeState {
	ns := &NodeState{}
	if raw, ok := annotations[AnnState]; ok {
		st := &StateInfo{Present: true, Raw: raw}
		var v struct {
			State string `json:"state"`
			Since string `json:"since"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil || v.State == "" || v.Since == "" {
			st.ParseError = "unparseable reboot-state: " + raw
			if v.Since != "" {
				if t, err := time.Parse(time.RFC3339, v.Since); err == nil {
					st.Since = t
				}
			}
		} else {
			st.State = State(v.State)
			if t, err := time.Parse(time.RFC3339, v.Since); err != nil {
				st.ParseError = "bad since: " + v.Since
			} else {
				st.Since = t
			}
		}
		ns.State = st
	}
	if raw, ok := annotations[AnnRequest]; ok {
		ri := &RequestInfo{Present: true}
		var v struct {
			ID    string `json:"id"`
			By    string `json:"by"`
			Force bool   `json:"force"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			ri.ParseError = "unparseable reboot-request"
			// force defaults to false: never assume force from a corrupt
			// annotation (3.3.3).
		} else {
			ri.ID, ri.By, ri.Force = v.ID, v.By, v.Force
		}
		ns.Request = ri
	}
	if raw, ok := annotations[AnnExec]; ok {
		ei := &ExecInfo{Present: true}
		var v struct {
			IssuedAt       string  `json:"issuedAt"`
			BootID         string  `json:"bootId"`
			Attempt        int     `json:"attempt"`
			ExecutorPodUID string  `json:"executorPodUID"`
			ConfirmedAt    *string `json:"confirmedAt"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil || v.IssuedAt == "" {
			ei.ParseError = "unparseable reboot-exec"
		} else {
			if t, err := time.Parse(time.RFC3339, v.IssuedAt); err != nil {
				ei.ParseError = "bad issuedAt"
			} else {
				ei.IssuedAt = t
				ei.BootID = v.BootID
				ei.Attempt = v.Attempt
				ei.ExecutorPodUID = v.ExecutorPodUID
				if v.ConfirmedAt != nil {
					if t, err := time.Parse(time.RFC3339, *v.ConfirmedAt); err == nil {
						ei.ConfirmedAt = &t
					}
				}
			}
		}
		ns.Exec = ei
	}
	if raw, ok := annotations[AnnStatus]; ok {
		si := &StatusInfo{Present: true}
		var v struct {
			BlockedBy    []string `json:"blockedBy"`
			Error        string   `json:"error"`
			CordonedPrev *bool    `json:"cordonedPrev"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			si.ParseError = "unparseable reboot-status"
			// fields render as absent; cordonedPrev is UNREADABLE, which
			// callers must treat as "do not uncordon" (3.3.2).
		} else {
			si.BlockedBy = v.BlockedBy
			si.Error = v.Error
			si.CordonedPrev = v.CordonedPrev
		}
		ns.Status = si
	}
	return ns
}

// --- Serialization -------------------------------------------------------

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// StateValue serializes reboot-state (state+since are one atomic unit).
func StateValue(state State, since time.Time) string {
	return marshal(struct {
		State string `json:"state"`
		Since string `json:"since"`
	}{State: string(state), Since: since.UTC().Format(time.RFC3339)})
}

// RequestValue serializes reboot-request ("by" omitted when absent).
func RequestValue(id, by string, force bool) string {
	v := struct {
		ID    string `json:"id"`
		By    string `json:"by,omitempty"`
		Force bool   `json:"force"`
	}{id, by, force}
	return marshal(v)
}

// ExecValue serializes reboot-exec (confirmedAt omitted when nil).
func ExecValue(e ExecInfo) string {
	v := struct {
		IssuedAt       string  `json:"issuedAt"`
		BootID         string  `json:"bootId"`
		Attempt        int     `json:"attempt"`
		ExecutorPodUID string  `json:"executorPodUID"`
		ConfirmedAt    *string `json:"confirmedAt,omitempty"`
	}{}
	v.IssuedAt = e.IssuedAt.UTC().Format(time.RFC3339)
	v.BootID = e.BootID
	v.Attempt = e.Attempt
	v.ExecutorPodUID = e.ExecutorPodUID
	if e.ConfirmedAt != nil {
		s := e.ConfirmedAt.UTC().Format(time.RFC3339)
		v.ConfirmedAt = &s
	}
	return marshal(v)
}

// StatusValue serializes reboot-status with only the relevant fields;
// returns "" when no field remains (caller must delete the key).
func StatusValue(s StatusInfo) string {
	v := struct {
		BlockedBy    []string `json:"blockedBy,omitempty"`
		Error        string   `json:"error,omitempty"`
		CordonedPrev *bool    `json:"cordonedPrev,omitempty"`
	}{}
	v.BlockedBy = s.BlockedBy
	v.Error = s.Error
	v.CordonedPrev = s.CordonedPrev
	if v.BlockedBy == nil && v.Error == "" && v.CordonedPrev == nil {
		return ""
	}
	return marshal(v)
}

// MergeStatus is the RMW step for the shared reboot-status annotation
// (3.3.2): parse the fresh value, apply only the caller's field changes,
// re-serialize. A corrupt fresh value renders as all fields absent.
func MergeStatus(freshRaw string, mutate func(*StatusInfo)) string {
	si := &StatusInfo{Present: freshRaw != ""}
	if freshRaw != "" {
		var v struct {
			BlockedBy    []string `json:"blockedBy"`
			Error        string   `json:"error"`
			CordonedPrev *bool    `json:"cordonedPrev"`
		}
		if err := json.Unmarshal([]byte(freshRaw), &v); err != nil {
			si.ParseError = "unparseable reboot-status"
		} else {
			si.BlockedBy = v.BlockedBy
			si.Error = v.Error
			si.CordonedPrev = v.CordonedPrev
		}
	}
	mutate(si)
	return StatusValue(*si)
}

// --- Patch builders (merge-patch+json, always with fresh resourceVersion) -

// annotationsPatch returns the metadata.annotations merge-patch section.
// A nil value deletes the annotation key.
func annotationsPatch(rv string, anns map[string]any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"resourceVersion": rv,
			"annotations":     anns,
		},
	}
}

// AdmissionPatch (API, PLAN 3.3.2): fresh requested state + fresh request,
// exec and status cleared atomically.
func AdmissionPatch(rv string, since time.Time, reqID, by string, force bool) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnState:   StateValue(Requested, since),
		AnnRequest: RequestValue(reqID, by, force),
		AnnExec:    nil,
		AnnStatus:  nil,
	})
}

// EnqueuePatch (update plan, PLAN-M2 3.8): set reboot-state to requested
// WITHOUT a reboot-request (so M1 treats it as force=false, PDB-gated —
// not forced) and clear exec and status atomically. The M1 orchestrator
// then drains/reboots/confirms it like any queued reboot.
func EnqueuePatch(rv string, since time.Time) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnState:   StateValue(Requested, since),
		AnnRequest: nil,
		AnnExec:    nil,
		AnnStatus:  nil,
	})
}

// EnqueueBuild returns a BuildFunc that enqueues a node into the M1 reboot
// queue (reboot-state := requested) only when the node is NOT already in
// the reboot lifecycle on the fresh node (a node mid-drain/reboot or
// already queued is never clobbered; a concurrent operator request is
// re-evaluated, never overwritten).
func EnqueueBuild(since time.Time) BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		st := Parse(node.Metadata.Annotations)
		if st.InLifecycle() {
			return nil, false
		}
		return EnqueuePatch(node.Metadata.ResourceVersion, since), true
	}
}

// ClearStaleRebootStateBuild (update plan, BUG 12): reset a node's stale
// terminal M1 reboot state (completed or failed) back to the resting state —
// all four reboot annotations cleared — at plan start. A stale completed or
// failed would otherwise be read by the plan's first verify as "came up on
// the wrong kernel" / "failed its reboot" and cancel the plan in the very
// cycle it is created (before the plan enqueues the member). Only a present,
// uncorrupt terminal state is cleared; a node that is absent (nothing to do),
// queued (requested), or in flight (draining/rebooting/corrupt) is left alone.
func ClearStaleRebootStateBuild() BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		st := Parse(node.Metadata.Annotations)
		if st.State == nil || !st.State.Present || st.State.ParseError != "" {
			return nil, false
		}
		if st.State.State != Completed && st.State.State != Failed {
			return nil, false
		}
		return ClearPatch(node.Metadata.ResourceVersion, false), true
	}
}

// ToDrainingPatch (orchestrator): state+status(+cordonedPrev) and the cordon
// in one atomic patch; blockedBy is dropped from the fresh status.
func ToDrainingPatch(rv string, now time.Time, freshStatusRaw string, wasCordoned bool) map[string]any {
	status := MergeStatus(freshStatusRaw, func(s *StatusInfo) {
		s.BlockedBy = nil
		if wasCordoned {
			t := true
			s.CordonedPrev = &t
		}
	})
	ann := map[string]any{
		AnnState: StateValue(Draining, now),
	}
	if status != "" {
		ann[AnnStatus] = status
	} else {
		ann[AnnStatus] = nil
	}
	p := annotationsPatch(rv, ann)
	p["spec"] = map[string]any{"unschedulable": true}
	return p
}

// ToRebootingPatch (orchestrator).
func ToRebootingPatch(rv string, now time.Time) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnState: StateValue(Rebooting, now),
	})
}

// ToCompletedPatch (orchestrator).
func ToCompletedPatch(rv string, now time.Time) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnState: StateValue(Completed, now),
	})
}

// ToFailedPatch (orchestrator or local pod): state + error in the SAME
// patch (3.3.2: whoever wins the state patch owns the error).
func ToFailedPatch(rv string, now time.Time, freshStatusRaw, errMsg string) map[string]any {
	status := MergeStatus(freshStatusRaw, func(s *StatusInfo) {
		s.Error = errMsg
	})
	return annotationsPatch(rv, map[string]any{
		AnnState:  StateValue(Failed, now),
		AnnStatus: statusOrNil(status),
	})
}

func statusOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// IssuedExecPatch (local pod): the one-patch record just before nsenter.
func IssuedExecPatch(rv string, e ExecInfo) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnExec: ExecValue(e),
	})
}

// ConfirmExecPatch (local pod): fresh exec + confirmedAt.
func ConfirmExecPatch(rv string, e ExecInfo, confirmedAt time.Time) map[string]any {
	e.ConfirmedAt = &confirmedAt
	return annotationsPatch(rv, map[string]any{
		AnnExec: ExecValue(e),
	})
}

// ClearPatch (any pod, PLAN 3.3.2): all four keys to null in one patch;
// unschedulable=false only per the caller's rule.
func ClearPatch(rv string, uncordon bool) map[string]any {
	p := annotationsPatch(rv, map[string]any{
		AnnState:   nil,
		AnnRequest: nil,
		AnnExec:    nil,
		AnnStatus:  nil,
	})
	if uncordon {
		p["spec"] = map[string]any{"unschedulable": false}
	}
	return p
}

// StatusPatch (orchestrator): RMW write of reboot-status only (3.3.2).
// Used for the per-cycle blockedBy update while requested.
func StatusPatch(rv string, freshRaw string, mutate func(*StatusInfo)) map[string]any {
	status := MergeStatus(freshRaw, mutate)
	return annotationsPatch(rv, map[string]any{
		AnnStatus: statusOrNil(status),
	})
}

// UncordonPatch (orchestrator's idempotent uncordon, never inside a
// transition patch).
func UncordonPatch(rv string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"resourceVersion": rv},
		"spec":     map[string]any{"unschedulable": false},
	}
}

// --- Conditional-transition patching (3.3.2) -----------------------------

// ErrAbort is returned when, on a fresh re-read, the transition's
// precondition no longer holds: nothing is written.
var ErrAbort = errors.New("nodestate: transition precondition no longer holds")

// BuildFunc builds a patch from a fresh node. It returns ok=false when the
// transition's precondition no longer holds against the fresh node
// (the caller must abort without writing).
type BuildFunc func(node *kube.Node) (map[string]any, bool)

// PatchTransition re-reads the node, evaluates build against it, and
// applies the returned merge-patch. On 409 it re-reads and re-evaluates
// build against the fresh node (immediately, within the caller's cycle)
// up to maxAttempts; when build reports the precondition no longer holds
// it aborts with ErrAbort without writing (3.3.2, failed-vs-failed and
// failed-vs-completed races).
func PatchTransition(ctx context.Context, c *kube.Client, nodeName string, maxAttempts int, build BuildFunc) error {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		node, err := c.GetNode(ctx, nodeName)
		if err != nil {
			return err
		}
		patch, ok := build(node)
		if !ok {
			return ErrAbort
		}
		if err := c.PatchNode(ctx, nodeName, patch); err != nil {
			if kube.IsConflict(err) {
				continue // re-read and re-evaluate on the fresh node
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("nodestate: %s: %d attempts, still conflicting", nodeName, maxAttempts)
}
