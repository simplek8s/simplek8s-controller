package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/config"
	"github.com/simplek8s/simplek8s-controller/internal/cron"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

const testToken = "test-token"

type harness struct {
	t      *testing.T
	fake   *kubetest.FakeAPI
	server *httptest.Server
	kc     *kube.Client
	now    time.Time
	feat   config.Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := fake.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h := &harness{t: t, fake: fake, kc: kc, now: now, feat: config.Defaults()}
	// Pre-window tests keep M1 admission behavior: an always-open
	// window. Window tests mutate h.feat directly.
	open, err := cron.Parse("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	h.feat.RebootWindows = []cron.Schedule{open}
	h.feat.RebootWindowGrace = 5 * time.Minute
	srv := New(Config{
		Kube:         kc,
		PodNamespace: "default",
		PodSelector:  map[string]string{"app": "simplek8s-controller"},
		Now:          func() time.Time { return now },
		Features:     func() config.Config { return h.feat },
	}, testToken)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	h.server = ts
	return h
}

// node registers a Ready node without reboot state.
func (h *harness) node(name string) {
	h.fake.SetNode(name, nil, false, nil, "uid-"+name)
	h.controllerPod(name)
}

// controllerPod registers a Running controller pod on the node.
func (h *harness) controllerPod(node string) {
	h.fake.AddPod(&kubetest.Pod{
		Name: "skc-" + node, Namespace: "default", NodeName: node,
		Phase: "Running", Labels: map[string]string{"app": "simplek8s-controller"},
	})
}

// do issues a request and returns the status plus the decoded body
// (JSON object or array).
func (h *harness) do(method, path, token string, body any) (int, any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// postReboots is a typed POST /api/v1/reboots.
func (h *harness) postReboots(body []byte) (int, RebootResponse) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/reboots", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out RebootResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAuth(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(http.MethodGet, "/api/v1/reboots", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/v1/reboots", "wrong", nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/v1/reboots", testToken, nil); code != http.StatusOK {
		t.Errorf("good token: got %d, want 200", code)
	}
	// probes are unauthenticated
	if code, _ := h.do(http.MethodGet, "/livez", "", nil); code != http.StatusOK {
		t.Errorf("/livez: got %d, want 200", code)
	}
	if code, _ := h.do(http.MethodGet, "/readyz", "", nil); code != http.StatusOK {
		t.Errorf("/readyz: got %d, want 200", code)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"10.244.0.5:8080", false},
		{"192.0.2.5:8080", false},
		{"not-an-addr", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestAuthTokenless (TODO 15, optional auth): without a token the API
// serves loopback clients (the port-forward path) with no bearer, and
// rejects direct cluster traffic with 403.
func TestAuthTokenless(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := fake.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Kube: kc}, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// httptest dials 127.0.0.1: no bearer needed.
	resp, err := http.Get(ts.URL + "/api/v1/reboots")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback without token: got %d, want 200", resp.StatusCode)
	}

	// Direct cluster traffic (non-loopback RemoteAddr) is rejected.
	for _, remote := range []string{"10.244.0.5:1234", "[fd00::5]:1234"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/reboots", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("remote %s without token: got %d, want 403", remote, rec.Code)
		}
	}
}

func TestReadyzUnhealthy(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	creds, _ := kubetest.MakeCreds(t)
	kc, _ := fake.Client(creds.Dir)
	srv := New(Config{Kube: kc, Ready: func() bool { return false }}, testToken)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz: got %d, want 503", resp.StatusCode)
	}
}

func TestPostHappyPath(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	code, resp := h.postReboots([]byte(`{"nodes":["w1"],"force":true,"requestedBy":"jls"}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "w1" || len(resp.Rejected) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
	var st struct {
		State string `json:"state"`
		Since string `json:"since"`
	}
	if err := json.Unmarshal([]byte(h.fake.NodeAnnotation("w1", nodestate.AnnState)), &st); err != nil {
		t.Fatalf("state annotation: %v", err)
	}
	if st.State != "requested" || st.Since != h.now.UTC().Format(time.RFC3339) {
		t.Errorf("state = %+v", st)
	}
	var req struct {
		ID    string `json:"id"`
		By    string `json:"by"`
		Force bool   `json:"force"`
	}
	if err := json.Unmarshal([]byte(h.fake.NodeAnnotation("w1", nodestate.AnnRequest)), &req); err != nil {
		t.Fatalf("request annotation: %v", err)
	}
	if req.ID == "" || req.By != "jls" || !req.Force {
		t.Errorf("request = %+v", req)
	}
	if h.fake.NodeAnnotation("w1", nodestate.AnnExec) != "" || h.fake.NodeAnnotation("w1", nodestate.AnnStatus) != "" {
		t.Error("stale exec/status not cleared")
	}
}

func TestPostPartial(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	h.node("w2")
	h.fake.SetNodeReady("w2", false)
	h.node("w3")
	h.fake.RemovePod("default", "skc-w3") // no controller pod on w3

	code, resp := h.postReboots([]byte(`{"nodes":["w1","w2","w3"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "w1" {
		t.Errorf("accepted = %v", resp.Accepted)
	}
	got := map[string]int{}
	for _, rj := range resp.Rejected {
		got[rj.Node] = rj.Code
	}
	if got["w2"] != http.StatusUnprocessableEntity || got["w3"] != http.StatusUnprocessableEntity {
		t.Errorf("rejected = %v (want w2=422, w3=422)", resp.Rejected)
	}
}

func TestPostReRequestAndConflicts(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	h.node("w2")
	h.node("w3")
	h.node("w4")
	// w2: completed with stale exec/status -> re-request clears them
	h.fake.SetNode("w2", map[string]string{
		nodestate.AnnState:  nodestate.StateValue(nodestate.Completed, h.now),
		nodestate.AnnExec:   nodestate.ExecValue(nodestate.ExecInfo{IssuedAt: h.now, BootID: "abc", Attempt: 1, ExecutorPodUID: "p1"}),
		nodestate.AnnStatus: `{"error":"boom"}`,
	}, false, nil, "uid-w2")
	h.controllerPod("w2")
	// w3: draining -> 409
	h.fake.SetNode("w3", map[string]string{
		nodestate.AnnState: nodestate.StateValue(nodestate.Draining, h.now),
	}, true, nil, "uid-w3")
	// w4: corrupt -> 409
	h.fake.SetNode("w4", map[string]string{
		nodestate.AnnState: `{"state":"bogus`,
	}, false, nil, "uid-w4")

	code, resp := h.postReboots([]byte(`{"nodes":["w1","w2","w3","w4","w9"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	got := map[string]int{}
	for _, rj := range resp.Rejected {
		got[rj.Node] = rj.Code
	}
	if got["w3"] != http.StatusConflict || got["w4"] != http.StatusConflict || got["w9"] != http.StatusNotFound {
		t.Errorf("rejected = %v (want w3=409, w4=409, w9=404)", resp.Rejected)
	}
	for _, a := range resp.Accepted {
		if a != "w1" && a != "w2" {
			t.Errorf("unexpected accepted %q", a)
		}
	}
	// w2's stale exec/status must be gone after the re-request
	if h.fake.NodeAnnotation("w2", nodestate.AnnExec) != "" {
		t.Error("w2: stale exec not cleared on re-request")
	}
	if h.fake.NodeAnnotation("w2", nodestate.AnnStatus) != "" {
		t.Error("w2: stale status not cleared on re-request")
	}
}

func TestPostStar(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	h.node("w2")
	code, resp := h.postReboots([]byte(`{"nodes":["*"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(resp.Accepted) != 2 || len(resp.Rejected) != 0 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestPostMalformed(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	code, _ := h.postReboots([]byte(`not json`))
	if code != http.StatusBadRequest {
		t.Errorf("bad body: got %d, want 400", code)
	}
	code, _ = h.postReboots([]byte(`{"nodes":[]}`))
	if code != http.StatusBadRequest {
		t.Errorf("empty nodes: got %d, want 400", code)
	}
}

func TestGetList(t *testing.T) {
	h := newHarness(t)
	h.node("w1") // no state: not listed
	h.fake.SetNode("w2", map[string]string{
		nodestate.AnnState:   nodestate.StateValue(nodestate.Draining, h.now),
		nodestate.AnnRequest: nodestate.RequestValue("req-1", "jls", true),
		nodestate.AnnStatus:  `{"blockedBy":["ns1/pdb1","ns2/pdb2"]}`,
		nodestate.AnnExec:    nodestate.ExecValue(nodestate.ExecInfo{IssuedAt: h.now, BootID: "abc", Attempt: 1, ExecutorPodUID: "p1"}),
	}, true, nil, "uid-w2")
	h.fake.SetNodeReady("w2", false)
	h.fake.SetNode("w3", map[string]string{
		nodestate.AnnState: `{"state":"bogus`,
	}, false, nil, "uid-w3")

	code, body := h.do(http.MethodGet, "/api/v1/reboots", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var items []Entry
	if err := json.Unmarshal(marshal(body), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v (want w2, w3)", items)
	}
	byName := map[string]Entry{}
	for _, e := range items {
		byName[e.Node] = e
	}
	w2 := byName["w2"]
	if w2.State != "draining" || w2.By != "jls" || !w2.Force || w2.Ready {
		t.Errorf("w2 = %+v", w2)
	}
	if len(w2.BlockedBy) != 2 || w2.BlockedBy[0] != "ns1/pdb1" {
		t.Errorf("w2.BlockedBy = %v", w2.BlockedBy)
	}
	if w2.IssuedAt == "" {
		t.Error("w2.IssuedAt empty")
	}
	if byName["w3"].ParseError == "" {
		t.Error("w3: want parseError")
	}
	if _, ok := byName["w1"]; ok {
		t.Error("w1 listed without reboot state")
	}
}

func TestGetSingle(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	h.fake.SetNode("w2", map[string]string{nodestate.AnnState: `{"state":"bogus`}, false, nil, "uid-w2")

	if code, _ := h.do(http.MethodGet, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNotFound {
		t.Errorf("no state: got %d, want 404", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/v1/reboots/unknown", testToken, nil); code != http.StatusNotFound {
		t.Errorf("unknown node: got %d, want 404", code)
	}
	code, body := h.do(http.MethodGet, "/api/v1/reboots/w2", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("corrupt: got %d, want 200", code)
	}
	var e Entry
	if err := json.Unmarshal(marshal(body), &e); err != nil {
		t.Fatal(err)
	}
	if e.ParseError == "" {
		t.Error("corrupt: want parseError, got 404-less entry without it")
	}
}

func TestDeleteRequestedKeepsCordon(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState:   nodestate.StateValue(nodestate.Requested, h.now),
		nodestate.AnnRequest: nodestate.RequestValue("req-1", "", false),
	}, true, nil, "uid-w1") // operator cordoned before requesting
	h.controllerPod("w1")

	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	for _, k := range []string{nodestate.AnnState, nodestate.AnnRequest, nodestate.AnnExec, nodestate.AnnStatus} {
		if h.fake.NodeAnnotation("w1", k) != "" {
			t.Errorf("%s not cleared", k)
		}
	}
	n, _ := h.fake.GetNode("w1")
	if !n["spec"].(map[string]any)["unschedulable"].(bool) {
		t.Error("requested: DELETE must not touch spec.unschedulable")
	}
}

func TestDeleteDrainingConflict(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState: nodestate.StateValue(nodestate.Draining, h.now),
	}, true, nil, "uid-w1")
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
}

func TestDeleteCompletedPreservesCordon(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState:   nodestate.StateValue(nodestate.Completed, h.now),
		nodestate.AnnRequest: nodestate.RequestValue("req-1", "", false),
		nodestate.AnnStatus:  `{"cordonedPrev":true}`,
	}, true, nil, "uid-w1")
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	n, _ := h.fake.GetNode("w1")
	if !n["spec"].(map[string]any)["unschedulable"].(bool) {
		t.Error("cordonedPrev=true: cordon must be preserved")
	}
}

func TestDeleteCompletedUncordons(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState:   nodestate.StateValue(nodestate.Completed, h.now),
		nodestate.AnnRequest: nodestate.RequestValue("req-1", "", false),
	}, true, nil, "uid-w1") // controller cordoned, cordonedPrev known absent
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	n, _ := h.fake.GetNode("w1")
	if n["spec"].(map[string]any)["unschedulable"].(bool) {
		t.Error("cordonedPrev absent: cordon must be lifted")
	}
}

func TestDeleteCompletedCorruptStatusPreservesCordon(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState:  nodestate.StateValue(nodestate.Completed, h.now),
		nodestate.AnnStatus: `{"cordonedPrev":`, // corrupt: unreadable
	}, true, nil, "uid-w1")
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	n, _ := h.fake.GetNode("w1")
	if !n["spec"].(map[string]any)["unschedulable"].(bool) {
		t.Error("unreadable cordonedPrev: cordon must be preserved")
	}
}

func TestDeleteCorruptState(t *testing.T) {
	h := newHarness(t)
	h.fake.SetNode("w1", map[string]string{
		nodestate.AnnState:   `{"state":"bogus`,
		nodestate.AnnRequest: `{"id":"req-1"}`,
		nodestate.AnnExec:    `{"issuedAt":"2026-01-02T03:04:05Z"}`,
	}, true, nil, "uid-w1")
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (guaranteed escape)", code)
	}
	for _, k := range []string{nodestate.AnnState, nodestate.AnnRequest, nodestate.AnnExec, nodestate.AnnStatus} {
		if h.fake.NodeAnnotation("w1", k) != "" {
			t.Errorf("%s not cleared", k)
		}
	}
	n, _ := h.fake.GetNode("w1")
	if !n["spec"].(map[string]any)["unschedulable"].(bool) {
		t.Error("corrupt state: no auto-uncordon")
	}
}

func TestDeleteNoState(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/w1", testToken, nil); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if code, _ := h.do(http.MethodDelete, "/api/v1/reboots/unknown", testToken, nil); code != http.StatusNotFound {
		t.Fatalf("unknown node: got %d, want 404", code)
	}
}

func TestAPIDown(t *testing.T) {
	h := newHarness(t)
	h.node("w1")
	h.fake.Close() // simulate the API server being unreachable

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/reboots/w1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET with API down: got %d, want 503", resp.StatusCode)
	}
	code, resp2 := h.postReboots([]byte(`{"nodes":["w1"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("POST with API down: got %d, want 202 (partial)", code)
	}
	if len(resp2.Rejected) != 1 || resp2.Rejected[0].Code != http.StatusServiceUnavailable {
		t.Errorf("rejected = %+v (want w1=503)", resp2.Rejected)
	}
}

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestPostEmptyWindowsRejects(t *testing.T) {
	h := newHarness(t)
	h.feat.RebootWindows = nil // empty = OFF
	h.node("w1")
	code, resp := h.postReboots([]byte(`{"nodes":["w1","w9"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (partial-batch envelope)", code)
	}
	if len(resp.Accepted) != 0 {
		t.Fatalf("accepted = %v, want none", resp.Accepted)
	}
	got := map[string]Rejection{}
	for _, rj := range resp.Rejected {
		got[rj.Node] = rj
	}
	// Unknown node + empty windows -> 422, not 404 (checked first).
	for _, name := range []string{"w1", "w9"} {
		rj, ok := got[name]
		if !ok {
			t.Fatalf("missing rejection for %s: %+v", name, resp.Rejected)
		}
		if rj.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s code = %d, want 422", name, rj.Code)
		}
		if !strings.Contains(rj.Reason, "NoWindowsConfigured") {
			t.Errorf("%s reason = %q, want NoWindowsConfigured", name, rj.Reason)
		}
	}
	// Nothing was queued.
	if ann := h.fake.NodeAnnotation("w1", nodestate.AnnState); ann != "" {
		t.Errorf("w1 state annotation = %q, want absent (not queued)", ann)
	}
}

func TestPostEmptyWindowsForcedBypasses(t *testing.T) {
	h := newHarness(t)
	h.feat.RebootWindows = nil // empty = OFF
	h.node("w1")
	code, resp := h.postReboots([]byte(`{"nodes":["w1"],"force":true}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "w1" || len(resp.Rejected) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestPostWindowsConfiguredAdmits(t *testing.T) {
	h := newHarness(t) // harness default: always-open window
	h.node("w1")
	code, resp := h.postReboots([]byte(`{"nodes":["w1"]}`))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if len(resp.Accepted) != 1 || resp.Accepted[0] != "w1" || len(resp.Rejected) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}
