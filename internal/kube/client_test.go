package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeCreds is a local temp creds dir (this package cannot import
// kubetest: it would cycle).
func makeCreds(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(d+"/token", []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d+"/ca.crt", []byte(testCAPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

const testCAPEM = `-----BEGIN CERTIFICATE-----
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

func newClientAt(t *testing.T, url string, sleep func(context.Context, time.Duration) error) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: url, CredsDir: makeCreds(t), Sleep: sleep})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEvictionStatusCodes(t *testing.T) {
	// 200 accepted / 429 denied (PDB) / 404 already gone — and 429 must
	// NOT be retried as server overload (PLAN 3.2).
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods/ok-pod/eviction"):
			atomic.AddInt32(&attempts, 1)
			w.Write([]byte(`{"kind":"Status","status":"Success"}`))
		case strings.HasSuffix(r.URL.Path, "/pods/denied-pod/eviction"):
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(429)
			w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"TooManyRequests","message":"pdb denies"}`))
		case strings.HasSuffix(r.URL.Path, "/pods/gone-pod/eviction"):
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(404)
			w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"NotFound"}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := newClientAt(t, srv.URL, nil)
	ctx := context.Background()

	if err := c.Evict(ctx, "default", "ok-pod"); err != nil {
		t.Fatalf("200 accepted: %v", err)
	}
	if err := c.Evict(ctx, "default", "denied-pod"); !IsTooManyRequests(err) {
		t.Fatalf("want 429 denial, got %v", err)
	}
	if err := c.Evict(ctx, "default", "gone-pod"); !IsNotFound(err) {
		t.Fatalf("want 404 gone, got %v", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Fatalf("eviction attempts = %d, want 3 (no 429 retry)", n)
	}
}

func TestMergePatchResourceVersionDiscipline(t *testing.T) {
	// 200 (fresh rv) / 409 (stale rv) / 200 (retry with fresh rv) —
	// the caller-visible contract of PatchNode (PLAN 3.3.2, M1).
	var rv int32 = 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || !strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") {
			w.WriteHeader(404)
			return
		}
		var patch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&patch)
		var want int32
		switch v := patch["metadata"].(map[string]any)["resourceVersion"].(type) {
		case string:
			n, _ := strconv.ParseInt(v, 10, 32)
			want = int32(n)
		case float64:
			want = int32(v)
		}
		if want != atomic.LoadInt32(&rv) {
			w.WriteHeader(409)
			w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Conflict"}`))
			return
		}
		atomic.AddInt32(&rv, 1) // server bumps rv on success
		w.Write([]byte(`{"metadata":{"name":"n1"}}`))
	}))
	defer srv.Close()
	c := newClientAt(t, srv.URL, nil)
	ctx := context.Background()

	if err := c.PatchNode(ctx, "n1", map[string]any{
		"metadata": map[string]any{"resourceVersion": "100"},
	}); err != nil {
		t.Fatalf("fresh patch: %v", err)
	}
	if err := c.PatchNode(ctx, "n1", map[string]any{
		"metadata": map[string]any{"resourceVersion": "100"},
	}); !IsConflict(err) {
		t.Fatalf("stale patch: want 409, got %v", err)
	}
	if err := c.PatchNode(ctx, "n1", map[string]any{
		"metadata": map[string]any{"resourceVersion": "101"},
	}); err != nil {
		t.Fatalf("retried patch: %v", err)
	}
}

func TestGetRetriesOn5xxAndHonorsRetryAfter(t *testing.T) {
	var n int32
	var sleeps []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"ServiceUnavailable"}`))
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	var mu sync.Mutex
	c := newClientAt(t, srv.URL, func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, d)
		mu.Unlock()
		return nil
	})
	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(nodes) != 0 || n != 3 {
		t.Fatalf("nodes=%d attempts=%d", len(nodes), n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != time.Second {
		t.Fatalf("Retry-After not honored: %v", sleeps)
	}
}

func TestListPagination(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		q := r.URL.Query()
		if q.Get("continue") == "" {
			items := []map[string]any{nodeItem("n1")}
			w.Write([]byte(`{"kind":"NodeList","metadata":{"continue":"page2"},"items":` + itemJSON(items) + `}`))
			return
		}
		if q.Get("continue") != "page2" {
			t.Errorf("unexpected continue token %q", q.Get("continue"))
		}
		items := []map[string]any{nodeItem("n2"), nodeItem("n3")}
		w.Write([]byte(`{"kind":"NodeList","metadata":{},"items":` + itemJSON(items) + `}`))
	}))
	defer srv.Close()
	c := newClientAt(t, srv.URL, nil)
	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || nodes[0].Metadata.Name != "n1" || nodes[2].Metadata.Name != "n3" {
		t.Fatalf("pages: %+v", nodes)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestTokenRereadPerRequest(t *testing.T) {
	// token rotation: the file is re-read on every request.
	creds := makeCreds(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL, CredsDir: creds})
	ctx := context.Background()
	_, _ = c.ListNodes(ctx)
	if err := os.WriteFile(creds+"/token", []byte("rotated-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = c.ListNodes(ctx)
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "Bearer ") || seen[1] != "Bearer rotated-token" {
		t.Fatalf("tokens: %v", seen)
	}
}

// leaseServer is a minimal coordination.k8s.io lease fake.
type leaseServer struct {
	mu     sync.Mutex
	rv     int
	leases map[string]map[string]any
}

func newLeaseServer() (*leaseServer, *httptest.Server) {
	l := &leaseServer{leases: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.handle(w, r)
	}))
	return l, srv
}

func (l *leaseServer) handle(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/apis/coordination.k8s.io/v1/namespaces/")
	switch r.Method {
	case http.MethodGet:
		if v, ok := l.leases[key]; ok {
			enc(w, 200, v)
		} else {
			enc(w, 404, map[string]any{"reason": "NotFound"})
		}
	case http.MethodPost:
		// create: POST to the collection path; the name comes from the body
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		if md, ok := v["metadata"].(map[string]any); ok {
			if name, _ := md["name"].(string); name != "" {
				key = strings.TrimSuffix(key, "/leases") + "/leases/" + name
			}
		}
		if _, exists := l.leases[key]; exists {
			enc(w, 409, map[string]any{"reason": "AlreadyExists"})
			return
		}
		l.rv++
		v["metadata"] = mergeMap(v["metadata"], map[string]any{"resourceVersion": itoa(l.rv)})
		l.leases[key] = v
		enc(w, 200, v)
	case http.MethodPut:
		cur, exists := l.leases[key]
		if !exists {
			enc(w, 404, map[string]any{"reason": "NotFound"})
			return
		}
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		if md, ok := v["metadata"].(map[string]any); ok {
			if rv, present := md["resourceVersion"]; present && itoaRV(cur) != rv {
				enc(w, 409, map[string]any{"reason": "Conflict"})
				return
			}
		}
		l.rv++
		v["metadata"] = mergeMap(v["metadata"], map[string]any{"resourceVersion": itoa(l.rv)})
		l.leases[key] = v
		enc(w, 200, v)
	}
}

func itoaRV(cur map[string]any) string {
	md, _ := cur["metadata"].(map[string]any)
	r, _ := md["resourceVersion"].(string)
	return r
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func mergeMap(a any, b map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := a.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func enc(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestLeaseVerbs(t *testing.T) {
	ls, srv := newLeaseServer()
	defer srv.Close()
	c := newClientAt(t, srv.URL, nil)
	ctx := context.Background()

	_, err := c.GetLease(ctx, "ns", "leader")
	if !IsNotFound(err) {
		t.Fatalf("want 404, got %v", err)
	}
	now := MicroTime(time.Now())
	le := &Lease{}
	le.Metadata.Name = "leader"
	le.Metadata.Namespace = "ns"
	le.Spec.HolderIdentity = "pod-a"
	le.Spec.RenewTime = &now
	if err := c.CreateLease(ctx, le); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.CreateLease(ctx, le); !IsConflict(err) {
		t.Fatalf("want 409, got %v", err)
	}
	got, _ := c.GetLease(ctx, "ns", "leader")
	got.Metadata.ResourceVersion = "stale"
	if err := c.UpdateLease(ctx, got); !IsConflict(err) {
		t.Fatalf("want 409, got %v", err)
	}
	got, _ = c.GetLease(ctx, "ns", "leader")
	got.Spec.HolderIdentity = "pod-b"
	if err := c.UpdateLease(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = c.GetLease(ctx, "ns", "leader")
	if got.Spec.HolderIdentity != "pod-b" {
		t.Fatalf("holder: %q", got.Spec.HolderIdentity)
	}
	ls.mu.Lock()
	if len(ls.leases) != 1 {
		ls.mu.Unlock()
		t.Fatal("lease store size")
	}
	ls.mu.Unlock()
}

func TestEventCreationInDefaultNamespace(t *testing.T) {
	// Node Events are cluster-scoped subjects: the API server requires
	// them in the "default" namespace.
	var mu sync.Mutex
	var events []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/namespaces/default/events") {
			enc(w, 404, map[string]any{"reason": "NotFound"})
			return
		}
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		events = append(events, v)
		mu.Unlock()
		enc(w, 200, v)
	}))
	defer srv.Close()
	c := newClientAt(t, srv.URL, nil)
	e := &Event{}
	e.Metadata.Namespace = "default"
	e.Metadata.GenerateName = "reboot-"
	e.InvolvedObject.Kind = "Node"
	e.InvolvedObject.Name = "n1"
	e.Reason = "RebootStarted"
	e.Message = "reboot issued"
	if err := c.CreateEvent(context.Background(), "default", e); err != nil {
		t.Fatalf("event in default namespace: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
}

func nodeItem(name string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "uid": name + "-uid"},
		"spec":     map[string]any{},
		"status":   map[string]any{"conditions": []map[string]any{{"type": "Ready", "status": "True"}}},
	}
}

func itemJSON(items []map[string]any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, _ := json.Marshal(it)
		b.Write(raw)
	}
	b.WriteByte(']')
	return b.String()
}
