// Package api implements the per-instance HTTP REST API (PLAN 3.9):
// POST/GET /api/v1/reboots, GET/DELETE /api/v1/reboots/{node},
// /livez and /readyz, guarded by a mandatory fixed bearer token.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// Config configures the API server.
type Config struct {
	Kube *kube.Client
	// PodNamespace is where the controller's pods run (POD_NAMESPACE);
	// used by admission check 3 (controller pod Running on the node).
	PodNamespace string
	// PodSelector identifies the controller's pods by label.
	PodSelector map[string]string
	Now         func() time.Time
	Log         *slog.Logger
	// Ready reports engine health for /readyz (PLAN 3.14).
	Ready func() bool
}

// Server is the HTTP API.
type Server struct {
	cfg   Config
	token string
}

// New builds the API server. token must be non-empty (main refuses to
// start without SIMPLEK8S_API_TOKEN, PLAN 3.9).
func New(cfg Config, token string) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Ready == nil {
		cfg.Ready = func() bool { return true }
	}
	return &Server{cfg: cfg, token: token}
}

// Handler wires the routes with the auth middleware. /livez and /readyz
// are unauthenticated (kubelet probes carry no token).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("POST /api/v1/reboots", s.auth(s.postReboots))
	mux.HandleFunc("GET /api/v1/reboots", s.auth(s.listReboots))
	mux.HandleFunc("GET /api/v1/reboots/{node}", s.auth(s.getReboot))
	mux.HandleFunc("DELETE /api/v1/reboots/{node}", s.auth(s.deleteReboot))
	return mux
}

// auth enforces "Authorization: Bearer <token>" (constant-time compare).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "Unauthorized", "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "Unauthorized", "invalid token")
			return
		}
		next(w, r)
	}
}

func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Ready() {
		writeJSON(w, map[string]any{"status": "ok"})
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "engine loop unhealthy")
}

// --- request / response types -------------------------------------------

// RebootRequest is the POST /api/v1/reboots body.
type RebootRequest struct {
	Nodes       []string `json:"nodes"`
	Force       bool     `json:"force"`
	RequestedBy string   `json:"requestedBy,omitempty"`
}

// Rejection is one per-node admission failure in the partial response.
type Rejection struct {
	Node   string `json:"node"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// RebootResponse is the partial admission result (always 202).
type RebootResponse struct {
	Accepted []string    `json:"accepted"`
	Rejected []Rejection `json:"rejected"`
}

// Entry is one node's reboot state as rendered by GET /reboots. The
// lifecycle fields come from the four annotations; Ready comes from
// Node.status (PLAN 3.9).
type Entry struct {
	Node        string   `json:"node"`
	State       string   `json:"state"`
	Since       string   `json:"since,omitempty"`
	Force       bool     `json:"force"`
	By          string   `json:"by,omitempty"`
	BlockedBy   []string `json:"blockedBy,omitempty"`
	Error       string   `json:"error,omitempty"`
	IssuedAt    string   `json:"issuedAt,omitempty"`
	ConfirmedAt string   `json:"confirmedAt,omitempty"`
	Ready       bool     `json:"ready"`
	ParseError  string   `json:"parseError,omitempty"`
}

// --- POST /api/v1/reboots -------------------------------------------------

func (s *Server) postReboots(w http.ResponseWriter, r *http.Request) {
	var req RebootRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	if len(req.Nodes) == 0 {
		writeErr(w, http.StatusBadRequest, "BadRequest", "nodes is required")
		return
	}
	for _, n := range req.Nodes {
		if n == "" {
			writeErr(w, http.StatusBadRequest, "BadRequest", "empty node name")
			return
		}
	}
	targets := req.Nodes
	if containsStar(req.Nodes) {
		nodes, err := s.cfg.Kube.ListNodes(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "API server unreachable")
			return
		}
		targets = nil
		for i := range nodes {
			targets = append(targets, nodes[i].Metadata.Name)
		}
	}
	now := s.cfg.Now()
	resp := RebootResponse{Accepted: []string{}, Rejected: []Rejection{}}
	for _, name := range targets {
		if code, reason := s.admit(r.Context(), name, req.Force, req.RequestedBy, now); code != 0 {
			resp.Rejected = append(resp.Rejected, Rejection{Node: name, Code: code, Reason: reason})
			continue
		}
		resp.Accepted = append(resp.Accepted, name)
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, resp)
}

func containsStar(nodes []string) bool {
	for _, n := range nodes {
		if n == "*" {
			return true
		}
	}
	return false
}

// admit runs the per-node admission checks (PLAN 3.9) and writes the
// atomic admission patch. It returns (0, "") on success, else the
// admission-check status code and a reason.
func (s *Server) admit(ctx context.Context, name string, force bool, by string, now time.Time) (int, string) {
	node, err := s.cfg.Kube.GetNode(ctx, name)
	if err != nil {
		if kube.IsNotFound(err) {
			return http.StatusNotFound, "node not found"
		}
		return http.StatusServiceUnavailable, "API server unreachable"
	}
	if !node.Ready() {
		return http.StatusUnprocessableEntity, "node not Ready"
	}
	if !s.controllerPodRunning(ctx, node) {
		return http.StatusUnprocessableEntity, "controller pod not Running on node"
	}
	st := nodestate.Parse(node.Metadata.Annotations)
	if !reRequestable(st) {
		return http.StatusConflict, "state is not re-requestable: " + describeState(st)
	}
	reqID, err := newRequestID(now)
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	err = nodestate.PatchTransition(ctx, s.cfg.Kube, name, 3, func(n *kube.Node) (map[string]any, bool) {
		fresh := nodestate.Parse(n.Metadata.Annotations)
		if !reRequestable(fresh) {
			return nil, false
		}
		return nodestate.AdmissionPatch(n.Metadata.ResourceVersion, now, reqID, by, force), true
	})
	if err != nil {
		if errors.Is(err, nodestate.ErrAbort) || kube.IsConflict(err) {
			return http.StatusConflict, "state changed during admission"
		}
		return http.StatusServiceUnavailable, "API server unreachable"
	}
	return 0, ""
}

// reRequestable: state absent, completed, or failed (PLAN 3.9 check 4).
// A corrupt reboot-state is never re-requestable (3.3.3).
func reRequestable(st *nodestate.NodeState) bool {
	if st.State == nil || !st.State.Present {
		return true
	}
	if st.State.ParseError != "" {
		return false
	}
	return st.State.State == nodestate.Completed || st.State.State == nodestate.Failed
}

func describeState(st *nodestate.NodeState) string {
	if st.State.ParseError != "" {
		return "corrupt"
	}
	return string(st.State.State)
}

// controllerPodRunning implements admission check 3.
func (s *Server) controllerPodRunning(ctx context.Context, node *kube.Node) bool {
	pods, err := s.cfg.Kube.ListPods(ctx)
	if err != nil {
		return false
	}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != node.Metadata.Name {
			continue
		}
		if s.cfg.PodNamespace != "" && p.Metadata.Namespace != s.cfg.PodNamespace {
			continue
		}
		if !labelsMatch(p.Metadata.Labels, s.cfg.PodSelector) {
			continue
		}
		if p.Running() {
			return true
		}
	}
	return false
}

func labelsMatch(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// newRequestID builds "<RFC3339 UTC>-<6 random hex>" (PLAN 3.3).
func newRequestID(now time.Time) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: random: %w", err)
	}
	return now.UTC().Format(time.RFC3339) + "-" + hex.EncodeToString(b), nil
}

// --- GET /api/v1/reboots --------------------------------------------------

func (s *Server) listReboots(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.cfg.Kube.ListNodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "API server unreachable")
		return
	}
	items := []Entry{}
	for i := range nodes {
		if e, ok := entry(&nodes[i]); ok {
			items = append(items, e)
		}
	}
	writeJSON(w, items)
}

func (s *Server) getReboot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")
	node, err := s.cfg.Kube.GetNode(r.Context(), name)
	if err != nil {
		if kube.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "NotFound", "no reboot state for node "+name)
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "API server unreachable")
		return
	}
	e, ok := entry(node)
	if !ok {
		writeErr(w, http.StatusNotFound, "NotFound", "no reboot state for node "+name)
		return
	}
	writeJSON(w, e)
}

// entry renders a node's reboot state; ok=false means the node has no
// reboot-state annotation (not listed). A corrupt state renders with
// ParseError (never a 500, 3.3.3).
func entry(n *kube.Node) (Entry, bool) {
	raw, ok := n.Metadata.Annotations[nodestate.AnnState]
	if !ok || raw == "" {
		return Entry{}, false
	}
	e := Entry{Node: n.Metadata.Name, Ready: n.Ready()}
	st := nodestate.Parse(n.Metadata.Annotations)
	if st.State.ParseError != "" {
		e.ParseError = st.State.ParseError
		return e, true
	}
	e.State = string(st.State.State)
	e.Since = st.State.Since.UTC().Format(time.RFC3339)
	if st.Request != nil && st.Request.Present && st.Request.ParseError == "" {
		e.Force = st.Request.Force
		e.By = st.Request.By
	}
	if st.Status != nil && st.Status.Present && st.Status.ParseError == "" {
		e.BlockedBy = st.Status.BlockedBy
		e.Error = st.Status.Error
	}
	if st.Exec != nil && st.Exec.Present && st.Exec.ParseError == "" {
		e.IssuedAt = st.Exec.IssuedAt.UTC().Format(time.RFC3339)
		if st.Exec.ConfirmedAt != nil {
			e.ConfirmedAt = st.Exec.ConfirmedAt.UTC().Format(time.RFC3339)
		}
	}
	return e, true
}

// --- DELETE /api/v1/reboots/{node} ----------------------------------------

// deleteReboot clears the four annotations (PLAN 3.9). Per-state
// semantics on the FRESH node each attempt; on 409 it re-reads and
// re-evaluates (the state may have moved out of draining).
func (s *Server) deleteReboot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")
	ctx := r.Context()
	for attempt := 0; attempt < 3; attempt++ {
		node, err := s.cfg.Kube.GetNode(ctx, name)
		if err != nil {
			if kube.IsNotFound(err) {
				writeErr(w, http.StatusNotFound, "NotFound", "node not found")
				return
			}
			writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "API server unreachable")
			return
		}
		st := nodestate.Parse(node.Metadata.Annotations)
		if st.State == nil || !st.State.Present {
			writeErr(w, http.StatusNotFound, "NotFound", "no reboot state for node "+name)
			return
		}
		var patch map[string]any
		switch {
		case st.State.ParseError != "":
			// Guaranteed escape (3.3.3): uniform clear, no auto-uncordon.
			patch = nodestate.ClearPatch(node.Metadata.ResourceVersion, false)
		case st.State.State == nodestate.Draining:
			writeErr(w, http.StatusConflict, "Conflict", "in-flight drain is not interruptible")
			return
		case st.State.State == nodestate.Rebooting || st.State.State == nodestate.Completed || st.State.State == nodestate.Failed:
			// Passed draining: the controller may have cordoned. Uncordon
			// only when cordonedPrev is KNOWN absent (3.3.2).
			patch = nodestate.ClearPatch(node.Metadata.ResourceVersion, cordonedPrevKnownAbsent(st))
		default: // Requested: the cordon, if any, is the operator's.
			patch = nodestate.ClearPatch(node.Metadata.ResourceVersion, false)
		}
		if err := s.cfg.Kube.PatchNode(ctx, name, patch); err != nil {
			if kube.IsConflict(err) {
				continue
			}
			writeErr(w, http.StatusServiceUnavailable, "ServiceUnavailable", "API server unreachable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeErr(w, http.StatusConflict, "Conflict", "resourceVersion conflict")
}

// cordonedPrevKnownAbsent reports whether reboot-status.cordonedPrev is
// known to be absent: the status annotation is absent or parseable and
// the field is not set. Unreadable (corrupt) status preserves the cordon.
func cordonedPrevKnownAbsent(st *nodestate.NodeState) bool {
	if st.Status == nil || !st.Status.Present {
		return true
	}
	if st.Status.ParseError != "" {
		return false
	}
	return st.Status.CordonedPrev == nil
}

// --- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "status": "Failure", "reason": reason, "message": msg, "code": code,
	})
}
