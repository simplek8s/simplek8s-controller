// Package kubetest provides an in-memory fake Kubernetes API server for
// unit tests (httptest). It implements the minimal surface the controller
// uses: node merge-patches with resourceVersion (409 on stale), pods,
// PDBs, evictions (200/429/404), leases, events, and node conditions.
package kubetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// Pod is a mutable fake pod.
type Pod struct {
	Name        string
	Namespace   string
	NodeName    string
	Phase       string
	Labels      map[string]string
	Annotations map[string]string
	Owners      []string // owner kinds, e.g. "DaemonSet", "Node", "ReplicaSet"
	Terminating bool     // deletionTimestamp set
	GracePeriod int64
}

// PDB is a mutable fake PDB.
type PDB struct {
	Name      string
	Namespace string
	Selector  *kube.LabelSelector
	Allowed   int32
}

// FakeAPI is an in-memory Kubernetes API server.
type FakeAPI struct {
	*httptest.Server

	mu        sync.Mutex
	rvCounter int
	nodes     map[string]map[string]any
	pods      []*Pod
	pdbs      []*PDB
	leases    map[string]map[string]any
	events    []map[string]any
	// EvictDeny decides whether an eviction is PDB-denied (429).
	// The default (nil) never denies.
	EvictDeny func(ns, pod string) bool
	// PodGone reports whether an eviction target is already deleted (404).
	PodGone func(ns, pod string) bool
	// FailNext lists errors to inject (shifted per attempt, per path suffix).
	FailNext map[string]int
	// DelayNext applies a delay to the next matching request (path suffix).
	DelayNext map[string]time.Duration
	// Down makes every request fail with connection refused semantics by
	// closing the server; re-open is done by the test.
	down bool
	// TraceBody, when set, receives each node merge-patch body (debug).
	TraceBody func(path string, body []byte)
}

// NewFakeAPI starts the server with no nodes.
func NewFakeAPI() *FakeAPI {
	f := &FakeAPI{
		nodes:     map[string]map[string]any{},
		leases:    map[string]map[string]any{},
		FailNext:  map[string]int{},
		DelayNext: map[string]time.Duration{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *FakeAPI) rv() int { f.rvCounter++; return f.rvCounter }

// SetNode registers (or replaces) a node object.
func (f *FakeAPI) SetNode(name string, anns map[string]string, unschedulable bool, labels map[string]string, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conds := []map[string]any{{"type": "Ready", "status": "True"}}
	node := map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"uid":             uid,
			"resourceVersion": strconv.Itoa(f.rv()),
			"annotations":     anns,
			"labels":          labels,
		},
		"spec":   map[string]any{"unschedulable": unschedulable},
		"status": map[string]any{"conditions": conds},
	}
	f.nodes[name] = node
}

// SetNodeReady flips the Ready condition of a node (bumps resourceVersion).
func (f *FakeAPI) SetNodeReady(name string, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[name]
	if !ok {
		return
	}
	st := n["status"].(map[string]any)
	status := "False"
	if ready {
		status = "True"
	}
	st["conditions"] = []map[string]any{{"type": "Ready", "status": status}}
	n["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(f.rv())
}

// RemoveNode deletes a node (simulates the node leaving the cluster).
func (f *FakeAPI) RemoveNode(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, name)
}

// GetNode returns a deep copy of a node.
func (f *FakeAPI) GetNode(name string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[name]
	if !ok {
		return nil, false
	}
	b, _ := json.Marshal(n)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, true
}

// NodeAnnotation returns a node annotation ("" if absent).
func (f *FakeAPI) NodeAnnotation(name, key string) string {
	n, ok := f.GetNode(name)
	if !ok {
		return ""
	}
	md, _ := n["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	s, _ := ann[key].(string)
	return s
}

// AddPod appends a pod.
func (f *FakeAPI) AddPod(p *Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods = append(f.pods, p)
}

// RemovePod deletes a pod by ns/name.
func (f *FakeAPI) RemovePod(ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.pods[:0]
	for _, p := range f.pods {
		if p.Namespace != ns || p.Name != name {
			out = append(out, p)
		}
	}
	f.pods = out
}

// AddPDB appends a PDB.
func (f *FakeAPI) AddPDB(p *PDB) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pdbs = append(f.pdbs, p)
}

// Events returns the recorded events.
func (f *FakeAPI) Events() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.events))
	copy(out, f.events)
	return out
}

// InjectFailure makes the next n requests whose path ends with suffix fail
// with 503.
func (f *FakeAPI) InjectFailure(suffix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailNext[suffix] += n
}

// Lease returns a copy of the lease.
func (f *FakeAPI) Lease(ns, name string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[ns+"/"+name]
	if !ok {
		return nil, false
	}
	b, _ := json.Marshal(l)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, true
}

// --- HTTP handling -------------------------------------------------------

func (f *FakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if d := f.delay(r.URL.Path); d > 0 {
		f.mu.Unlock()
		time.Sleep(d)
		f.mu.Lock()
	}
	if n := f.takeFail(r.URL.Path); n > 0 {
		w.Header().Set("Retry-After", "0")
		httpError(w, 503, "ServiceUnavailable", "injected failure")
		f.mu.Unlock()
		return
	}
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")

	switch {
	case path == "api/v1/nodes" && r.Method == http.MethodGet:
		f.listNodes(w, r)
	case strings.HasPrefix(path, "api/v1/nodes/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "api/v1/nodes/")
		if n, ok := f.nodes[name]; ok {
			writeJSON(w, n)
		} else {
			httpError(w, 404, "NotFound", "node "+name)
		}
	case strings.HasPrefix(path, "api/v1/nodes/") && r.Method == http.MethodPatch:
		f.patchNode(w, r, strings.TrimPrefix(path, "api/v1/nodes/"))
	case path == "api/v1/pods" && r.Method == http.MethodGet:
		f.listPods(w)
	case strings.HasPrefix(path, "/api/v1/namespaces/") && strings.HasSuffix(path, "/eviction"):
		f.evict(w, r, path)
	case strings.HasPrefix(path, "api/v1/namespaces/") && strings.Contains(path, "/pods/") && r.Method == http.MethodDelete:
		// Note: f.mu is already held by handle; delete inline.
		parts := strings.Split(strings.TrimPrefix(path, "api/v1/namespaces/"), "/")
		if len(parts) == 3 && parts[1] == "pods" {
			out := f.pods[:0]
			for _, p := range f.pods {
				if p.Namespace != parts[0] || p.Name != parts[2] {
					out = append(out, p)
				}
			}
			f.pods = out
			w.WriteHeader(200)
			writeJSON(w, map[string]any{"status": "Success"})
		} else {
			httpError(w, 404, "NotFound", path)
		}
	case path == "apis/policy/v1/poddisruptionbudgets" && r.Method == http.MethodGet:
		f.listPDBs(w)
	case strings.HasPrefix(path, "apis/coordination.k8s.io/v1/namespaces/") && r.Method == http.MethodGet:
		f.getLease(w, path)
	case strings.HasPrefix(path, "apis/coordination.k8s.io/v1/namespaces/") && r.Method == http.MethodPost:
		f.createLease(w, r, path)
	case strings.HasPrefix(path, "apis/coordination.k8s.io/v1/namespaces/") && r.Method == http.MethodPut:
		f.updateLease(w, r, path)
	case strings.HasPrefix(path, "api/v1/namespaces/") && strings.HasSuffix(path, "/events") && r.Method == http.MethodPost:
		f.createEvent(w, r, path)
	default:
		httpError(w, 404, "NotFound", "fake: no handler for "+r.Method+" "+r.URL.Path)
	}
}

func (f *FakeAPI) delay(path string) time.Duration {
	for suffix, d := range f.DelayNext {
		if strings.HasSuffix(path, suffix) {
			return d
		}
	}
	return 0
}

func (f *FakeAPI) takeFail(path string) int {
	for suffix, n := range f.FailNext {
		if strings.HasSuffix(path, suffix) && n > 0 {
			f.FailNext[suffix] = n - 1
			return 1
		}
	}
	return 0
}

func (f *FakeAPI) listNodes(w http.ResponseWriter, r *http.Request) {
	items := []map[string]any{}
	for _, n := range f.nodes {
		items = append(items, n)
	}
	writeJSON(w, map[string]any{"kind": "NodeList", "apiVersion": "v1", "items": items, "metadata": map[string]any{}})
}

func (f *FakeAPI) listPods(w http.ResponseWriter) {
	items := []map[string]any{}
	for _, p := range f.pods {
		md := map[string]any{"name": p.Name, "namespace": p.Namespace, "labels": p.Labels, "annotations": p.Annotations}
		if p.Terminating {
			md["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
		}
		owners := []map[string]any{}
		for _, o := range p.Owners {
			owners = append(owners, map[string]any{"kind": o, "name": o + "-x"})
		}
		if len(owners) > 0 {
			md["ownerReferences"] = owners
		}
		items = append(items, map[string]any{
			"metadata": md,
			"spec":     map[string]any{"nodeName": p.NodeName, "terminationGracePeriodSeconds": p.GracePeriod},
			"status":   map[string]any{"phase": p.Phase},
		})
	}
	writeJSON(w, map[string]any{"kind": "PodList", "apiVersion": "v1", "items": items, "metadata": map[string]any{}})
}

func (f *FakeAPI) listPDBs(w http.ResponseWriter) {
	items := []map[string]any{}
	for _, p := range f.pdbs {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": p.Name, "namespace": p.Namespace, "resourceVersion": "1"},
			"spec":     map[string]any{"selector": p.Selector},
			"status":   map[string]any{"disruptionsAllowed": p.Allowed},
		})
	}
	writeJSON(w, map[string]any{"kind": "PodDisruptionBudgetList", "apiVersion": "policy/v1", "items": items, "metadata": map[string]any{}})
}

// patchNode applies a JSON merge patch with resourceVersion check (409).
func (f *FakeAPI) patchNode(w http.ResponseWriter, r *http.Request, name string) {
	n, ok := f.nodes[name]
	if !ok {
		httpError(w, 404, "NotFound", name)
		return
	}
	rawBody, _ := io.ReadAll(r.Body)
	var patch map[string]any
	if err := json.Unmarshal(rawBody, &patch); err != nil {
		httpError(w, 400, "BadRequest", err.Error())
		return
	}
	if f.TraceBody != nil {
		f.TraceBody(r.URL.Path, rawBody)
	}
	md, _ := patch["metadata"].(map[string]any)
	if md != nil {
		if rv, present := md["resourceVersion"]; present {
			cur, _ := n["metadata"].(map[string]any)["resourceVersion"].(string)
			if fmt.Sprint(rv) != cur {
				httpError(w, 409, "Conflict", "resourceVersion conflict")
				return
			}
		}
	}
	merged := mergePatch(n, patch)
	merged["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(f.rv())
	f.nodes[name] = merged
	writeJSON(w, merged)
}

// asAnyMap normalizes the map kinds the fake stores (SetNode uses
// map[string]string) so merge patching recurses correctly.
func asAnyMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out, true
	}
	return nil, false
}

// mergePatch is RFC 7386 JSON merge patch.
func mergePatch(target, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if tm, ok := asAnyMap(out[k]); ok {
				out[k] = mergePatch(tm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (f *FakeAPI) evict(w http.ResponseWriter, r *http.Request, path string) {
	// /api/v1/namespaces/{ns}/pods/{pod}/eviction
	rest := strings.TrimPrefix(path, "api/v1/namespaces/")
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "pods" || parts[3] != "eviction" {
		httpError(w, 404, "NotFound", path)
		return
	}
	ns, pod := parts[0], parts[2]
	if f.PodGone != nil && f.PodGone(ns, pod) {
		httpError(w, 404, "NotFound", "pod already gone")
		return
	}
	if f.EvictDeny != nil && f.EvictDeny(ns, pod) {
		httpError(w, 429, "TooManyRequests", "disruptions allowed: 0")
		return
	}
	// accepted: mark terminating (it goes away "up to grace later"; tests
	// remove it explicitly to simulate the kubelet finishing deletion)
	for _, p := range f.pods {
		if p.Namespace == ns && p.Name == pod {
			p.Terminating = true
		}
	}
	writeJSON(w, map[string]any{"kind": "Status", "status": "Success"})
}

// leasePathParts returns (namespace, name) from a lease API path. For the
// collection path (POST create) the name is "".
func leasePathParts(path string) (ns, name string, ok bool) {
	rest := strings.TrimPrefix(path, "apis/coordination.k8s.io/v1/namespaces/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	ns = parts[0]
	name = strings.TrimPrefix(parts[1], "leases/")
	if name == "leases" || name == "" {
		return ns, "", true
	}
	return ns, name, true
}

func (f *FakeAPI) getLease(w http.ResponseWriter, path string) {
	ns, name, _ := leasePathParts(path)
	key := ns + "/" + name
	if l, ok := f.leases[key]; ok {
		writeJSON(w, l)
	} else {
		httpError(w, 404, "NotFound", key)
	}
}

func (f *FakeAPI) createLease(w http.ResponseWriter, r *http.Request, path string) {
	var l map[string]any
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		httpError(w, 400, "BadRequest", err.Error())
		return
	}
	// create: POST to the collection path; the name comes from the body
	ns, name, _ := leasePathParts(path)
	if name == "" {
		if md, ok := l["metadata"].(map[string]any); ok {
			name, _ = md["name"].(string)
		}
	}
	key := ns + "/" + name
	if _, exists := f.leases[key]; exists {
		httpError(w, 409, "AlreadyExists", key)
		return
	}
	l["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(f.rv())
	f.leases[key] = l
	writeJSON(w, l)
}

func (f *FakeAPI) updateLease(w http.ResponseWriter, r *http.Request, path string) {
	ns, name, _ := leasePathParts(path)
	key := ns + "/" + name
	cur, exists := f.leases[key]
	var l map[string]any
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		httpError(w, 400, "BadRequest", err.Error())
		return
	}
	md, _ := l["metadata"].(map[string]any)
	if !exists {
		httpError(w, 404, "NotFound", key)
		return
	}
	if md != nil {
		if rv, present := md["resourceVersion"]; present {
			curRV, _ := cur["metadata"].(map[string]any)["resourceVersion"].(string)
			if fmt.Sprint(rv) != curRV {
				httpError(w, 409, "Conflict", "lease conflict")
				return
			}
		}
	}
	l["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(f.rv())
	f.leases[key] = l
	writeJSON(w, l)
}

func (f *FakeAPI) createEvent(w http.ResponseWriter, r *http.Request, path string) {
	var e map[string]any
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		httpError(w, 400, "BadRequest", err.Error())
		return
	}
	e["metadata"] = map[string]any{"name": "event-" + strconv.Itoa(len(f.events)+1), "resourceVersion": strconv.Itoa(f.rv())}
	f.events = append(f.events, e)
	writeJSON(w, e)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "status": "Failure", "reason": reason, "message": msg, "code": code,
	})
}

// Client builds a kube.Client against the fake. The fake speaks plain
// HTTP, so no CA is configured; the client re-reads the token from
// CredsDir, which must contain a token file (tests use a temp dir).
func (f *FakeAPI) Client(credsDir string) (*kube.Client, error) {
	return kube.New(kube.Config{
		Endpoint: f.Server.URL,
		CredsDir: credsDir,
	})
}

// Creds is a temp serviceaccount-style creds dir.
type Creds struct{ Dir string }

// testCA is a throwaway self-signed certificate so kube.New can load a
// ca.crt (the fake API speaks plain HTTP and never verifies it).
const testCA = `-----BEGIN CERTIFICATE-----
MIIBVDCB+6ADAgECAgEBMAoGCCqGSM49BAMCMBIxEDAOBgNVBAMTB3Rlc3QtY2Ew
HhcNMjYwOTA1MDk0MDMyWhcNMjcwOTA1MTA0MDMyWjASMRAwDgYDVQQDEwd0ZXN0
LWNhMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEhEpQ1FdLHlc2YSdn1w2Lu8gm
znR0VfDxHuy6Bkz3eAL6tDTvZOBrjarDsf4QDNWsQ7b240O03NwbzcNi3rpTx6NC
MEAwDgYDVR0PAQH/BAQDAgIEMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFNn8
jqnwG7v9KrYRWDuP6kcKYP2IMAoGCCqGSM49BAMCA0gAMEUCIQDeaPq7KUGd3sj6
COqLP0PyJDOW0O1BNtPJp7hUATC1nwIgbpyKh4ow4nBUtcbxSY5fUYLPV/QHvu+O
9proYOqomjk=
-----END CERTIFICATE-----
`

// MakeCreds creates a temp serviceaccount-style dir with token and CA.
func MakeCreds(t *testing.T) (*Creds, error) {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(d+"/token", []byte("test-token"), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(d+"/ca.crt", []byte(testCA), 0o600); err != nil {
		return nil, err
	}
	return &Creds{Dir: d}, nil
}
