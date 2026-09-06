package nodestate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
)

type kubeNode = kube.Node

func statusRaw(n *kubeNode) string {
	if n.Metadata.Annotations != nil {
		return n.Metadata.Annotations[AnnStatus]
	}
	return ""
}

func mustParseJSON(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
}

func TestParseAndSerializeRoundtrip(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	anns := map[string]string{
		AnnState:   StateValue(Draining, now),
		AnnRequest: RequestValue("id-1", "alice", true),
		AnnExec:    ExecValue(ExecInfo{IssuedAt: now, BootID: "beef", Attempt: 1, ExecutorPodUID: "pod-uid"}),
		AnnStatus:  `{"blockedBy":["ns1/pdb1"],"cordonedPrev":true}`,
	}
	ns := Parse(anns)

	if !ns.InLifecycle() || ns.State.State != Draining || !ns.State.Since.Equal(now) {
		t.Fatalf("state: %+v", ns.State)
	}
	if ns.Request.ID != "id-1" || ns.Request.By != "alice" || !ns.Request.Force {
		t.Fatalf("request: %+v", ns.Request)
	}
	if ns.Exec.BootID != "beef" || ns.Exec.Attempt != 1 || ns.Exec.ExecutorPodUID != "pod-uid" {
		t.Fatalf("exec: %+v", ns.Exec)
	}
	if ns.Status.BlockedBy == nil || ns.Status.BlockedBy[0] != "ns1/pdb1" {
		t.Fatalf("status blockedBy: %+v", ns.Status)
	}
	if ns.Status.CordonedPrev == nil || !*ns.Status.CordonedPrev {
		t.Fatalf("status cordonedPrev: %+v", ns.Status)
	}
	if !ns.InFlight() {
		t.Fatal("draining must hold a slot")
	}

	// State+since atomic roundtrip.
	var sv struct {
		State string `json:"state"`
		Since string `json:"since"`
	}
	mustParseJSON(t, StateValue(Rebooting, now), &sv)
	if sv.State != "rebooting" || sv.Since != now.UTC().Format(time.RFC3339) {
		t.Fatalf("state value: %+v", sv)
	}
	// "by" omitted when absent.
	var rv struct {
		By string `json:"by"`
	}
	mustParseJSON(t, RequestValue("id-2", "", false), &rv)
	if rv.By != "" {
		t.Fatalf("by must be omitted: %+v", rv)
	}
}

func TestParseCorruptAnnotationsNeverPanic(t *testing.T) {
	// reboot-state corrupt => in-flight slot holder, parseError set (3.3.3).
	ns := Parse(map[string]string{AnnState: "{not json"})
	if ns.State == nil || !ns.State.Present || ns.State.ParseError == "" {
		t.Fatalf("corrupt state: %+v", ns.State)
	}
	if !ns.InFlight() {
		t.Fatal("corrupt state must hold a concurrency slot")
	}

	// request corrupt => force defaults false (never assume force).
	ns = Parse(map[string]string{AnnRequest: "garbage", AnnState: StateValue(Requested, time.Now())})
	if ns.Request == nil || ns.Request.ParseError == "" || ns.Request.Force {
		t.Fatalf("corrupt request: %+v", ns.Request)
	}

	// exec corrupt => fields absent, checks simply do not run.
	ns = Parse(map[string]string{AnnExec: "[1,2", AnnState: StateValue(Rebooting, time.Now())})
	if ns.Exec == nil || ns.Exec.ParseError == "" || !ns.Exec.Present {
		t.Fatalf("corrupt exec: %+v", ns.Exec)
	}

	// status corrupt => cordonedPrev UNREADABLE (not guessed absent).
	ns = Parse(map[string]string{AnnStatus: `"x"`, AnnState: StateValue(Failed, time.Now())})
	if ns.Status == nil || ns.Status.ParseError == "" {
		t.Fatalf("corrupt status: %+v", ns.Status)
	}
	if ns.Status.CordonedPrev != nil {
		t.Fatal("corrupt status cordonedPrev must be nil (unreadable)")
	}

	// missing fields in state JSON (state present, since missing).
	ns = Parse(map[string]string{AnnState: `{"state":"rebooting"}`})
	if ns.State.ParseError == "" {
		t.Fatal("missing since must set parseError")
	}
}

func TestMergeStatusRMWKeepsUntouchedFields(t *testing.T) {
	// Same-instant cross-field RMW on reboot-status (M2): two writers
	// patch different fields of the shared annotation; applying each
	// writer's change on top of the FRESH value must not lose the other's
	// field.
	fresh := `{"blockedBy":["ns/pdb"],"cordonedPrev":true,"error":"boom"}`

	// Writer A: local pod sets error (its own field) on top of fresh.
	a := MergeStatus(fresh, func(s *StatusInfo) { s.Error = "reboot did not take effect" })
	var av struct {
		BlockedBy    []string `json:"blockedBy"`
		Error        string   `json:"error"`
		CordonedPrev *bool    `json:"cordonedPrev"`
	}
	mustParseJSON(t, a, &av)
	if av.Error != "reboot did not take effect" || av.CordonedPrev == nil || !*av.CordonedPrev {
		t.Fatalf("writer A lost fields: %s", a)
	}

	// Writer B (orchestrator) then clears blockedBy on top of A's fresh value.
	b := MergeStatus(a, func(s *StatusInfo) { s.BlockedBy = nil })
	var bv struct {
		BlockedBy    []string `json:"blockedBy"`
		Error        string   `json:"error"`
		CordonedPrev *bool    `json:"cordonedPrev"`
	}
	mustParseJSON(t, b, &bv)
	if bv.BlockedBy != nil {
		t.Fatalf("blockedBy not cleared: %s", b)
	}
	if bv.Error != "reboot did not take effect" || bv.CordonedPrev == nil {
		t.Fatalf("writer B clobbered A's fields: %s", b)
	}

	// Corrupt fresh value renders as all fields absent (no panic).
	c := MergeStatus("{corrupt", func(s *StatusInfo) { s.Error = "x" })
	var cv struct {
		BlockedBy    []string `json:"blockedBy"`
		Error        string   `json:"error"`
		CordonedPrev *bool    `json:"cordonedPrev"`
	}
	mustParseJSON(t, c, &cv)
	if cv.Error != "x" || cv.BlockedBy != nil || cv.CordonedPrev != nil {
		t.Fatalf("corrupt fresh: %s", c)
	}

	// No remaining fields => empty value (caller deletes the key).
	if d := MergeStatus(`{"error":"x"}`, func(s *StatusInfo) { s.Error = "" }); d != "" {
		t.Fatalf("expected empty status value, got %q", d)
	}
}

func TestClearPatchNullsAllFourKeys(t *testing.T) {
	p := ClearPatch("42", true)
	md := p["metadata"].(map[string]any)
	ann := md["annotations"].(map[string]any)
	for _, k := range []string{AnnState, AnnRequest, AnnExec, AnnStatus} {
		if v, ok := ann[k]; !ok || v != nil {
			t.Fatalf("key %s must be null in clear patch: %v", k, ann)
		}
	}
	if md["resourceVersion"] != "42" {
		t.Fatalf("resourceVersion: %v", md["resourceVersion"])
	}
	spec := p["spec"].(map[string]any)
	if spec["unschedulable"] != false {
		t.Fatal("clear patch with uncordon must set unschedulable=false")
	}
	if _, has := ClearPatch("42", false)["spec"]; has {
		t.Fatal("clear patch without uncordon must not touch spec")
	}
}

// newFakeAndNode boots a fake API with one node carrying the given state.
func newFakeAndNode(t *testing.T, name string, anns map[string]string) (*kubetest.FakeAPI, *kubetest.Creds) {
	t.Helper()
	f := kubetest.NewFakeAPI()
	t.Cleanup(f.Close)
	f.SetNode(name, anns, false, nil, "uid-"+name)
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	return f, creds
}

func TestPatchTransitionAppliesAndAborts(t *testing.T) {
	f, creds := newFakeAndNode(t, "n1", map[string]string{
		AnnState: StateValue(Requested, time.Now()),
	})
	c, err := f.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Successful transition requested->draining with the cordon.
	err = PatchTransition(ctx, c, "n1", 3, func(n *kubeNode) (map[string]any, bool) {
		ns := Parse(n.Metadata.Annotations)
		if ns.State == nil || ns.State.State != Requested {
			return nil, false
		}
		return ToDrainingPatch(n.Metadata.ResourceVersion, time.Now(),
			statusRaw(n), false), true
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnState); !strings.Contains(got, "draining") {
		t.Fatalf("state after transition: %q", got)
	}

	// Now the precondition no longer holds (already draining): the loser
	// must abort WITHOUT writing (failed-vs-failed / failed-vs-completed
	// race, 3.3.2).
	err = PatchTransition(ctx, c, "n1", 3, func(n *kubeNode) (map[string]any, bool) {
		ns := Parse(n.Metadata.Annotations)
		if ns.State == nil || ns.State.State != Requested {
			return nil, false // precondition gone: abort, no write
		}
		return ToFailedPatch(n.Metadata.ResourceVersion, time.Now(), statusRaw(n), "stale"), true
	})
	if err != ErrAbort {
		t.Fatalf("want ErrAbort, got %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnStatus); strings.Contains(got, "stale") {
		t.Fatalf("aborted transition must not write error: %q", got)
	}
}

func TestPatchTransitionRetriesOn409(t *testing.T) {
	f, creds := newFakeAndNode(t, "n1", map[string]string{
		AnnState: StateValue(Requested, time.Now()),
	})
	c, err := f.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A concurrent writer bumps resourceVersion between our read and patch
	// (simulated by an extra status update right after each read inside
	// the build step). The transition must still succeed on retry.
	var read int
	err = PatchTransition(ctx, c, "n1", 3, func(n *kubeNode) (map[string]any, bool) {
		read++
		if read == 1 {
			// simulate the kubelet bumping resourceVersion concurrently
			f.SetNodeReady("n1", true)
		}
		ns := Parse(n.Metadata.Annotations)
		if ns.State == nil || ns.State.State != Requested {
			return nil, false
		}
		return ToRebootingPatch(n.Metadata.ResourceVersion, time.Now()), true
	})
	if err != nil {
		t.Fatalf("409 retry: %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnState); !strings.Contains(got, "rebooting") {
		t.Fatalf("state: %q", got)
	}
	if read != 2 {
		t.Fatalf("expected re-read after 409, reads=%d", read)
	}
}
