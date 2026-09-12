package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/config"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
)

// clock is a manually advanced clock for deterministic lease tests.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
}

func newTestEngine(t *testing.T, fake *kubetest.FakeAPI, cl *clock, identity string) *Engine {
	t.Helper()
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{
		CredsDir:           creds.Dir,
		APIEndpoint:        fake.Server.URL,
		Identity:           identity,
		NodeName:           "n1",
		LeaseNamespace:     "default",
		LeaseName:          LeaseName,
		ConfigMapNamespace: "default",
		ConfigMapName:      "simplek8s-controller",
		Now:                cl.Now,
		Sleep:              func(ctx context.Context, d time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLeaseAcquireRenewTakeover(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	creds, _ := kubetest.MakeCreds(t)
	c, err := fake.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	cl := newClock()
	ctx := context.Background()

	a := NewLeaser(c, "default", LeaseName, "pod-a/uid-a", cl.Now)
	if s := a.Sync(ctx); s != LeaseLeader {
		t.Fatalf("initial acquire: got %v, want LeaseLeader", s)
	}
	// Second sync within the renew interval: still leader, no update.
	if s := a.Sync(ctx); s != LeaseLeader {
		t.Fatalf("throttled renew: got %v", s)
	}
	// Past the renew interval: the lease's renewTime advances.
	l0, _ := fake.Lease("default", LeaseName)
	cl.Advance(LeaseRenewInterval + time.Second)
	if s := a.Sync(ctx); s != LeaseLeader {
		t.Fatalf("renew: got %v", s)
	}
	l1, _ := fake.Lease("default", LeaseName)
	if reflect.DeepEqual(l0["spec"], l1["spec"]) {
		t.Fatal("renewTime did not advance")
	}

	// A second pod sees the fresh foreign holder: not leader.
	b := NewLeaser(c, "default", LeaseName, "pod-b/uid-b", cl.Now)
	if s := b.Sync(ctx); s != LeaseNotLeader {
		t.Fatalf("foreign fresh holder: got %v", s)
	}
	// Stale (a died): b takes over.
	cl.Advance(LeaseStaleAfter + time.Second)
	if s := b.Sync(ctx); s != LeaseLeader {
		t.Fatalf("takeover: got %v", s)
	}
	// a returns: fresh foreign holder, it must stop deciding.
	if s := a.Sync(ctx); s != LeaseNotLeader {
		t.Fatalf("returned pod: got %v", s)
	}
	if a.Leader() {
		t.Fatal("a must not believe itself leader after b took over")
	}
}

func TestLeaseConflictClassification(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	creds, _ := kubetest.MakeCreds(t)
	c, _ := fake.Client(creds.Dir)
	cl := newClock()
	ctx := context.Background()

	a := NewLeaser(c, "default", LeaseName, "pod-a/uid-a", cl.Now)
	a.Sync(ctx)
	cl.Advance(LeaseStaleAfter + time.Second)
	// b takes over while a is "down".
	b := NewLeaser(c, "default", LeaseName, "pod-b/uid-b", cl.Now)
	if s := b.Sync(ctx); s != LeaseLeader {
		t.Fatalf("b takeover: %v", s)
	}
	// a's stale renewal conflicts (rv changed): it must classify as
	// not-leader, not as no-valid-leader.
	if s := a.Sync(ctx); s != LeaseNotLeader {
		t.Fatalf("conflict classification: got %v", s)
	}
}

func TestSplitBrainSingleDecider(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	fake.SetNode("n1", nil, false, nil, "uid-n1")
	cl := newClock()
	ctx := context.Background()

	var decA, decB int32
	a := newTestEngine(t, fake, cl, "pod-a/uid-a")
	b := newTestEngine(t, fake, cl, "pod-b/uid-b")
	a.Register(funcTask(func(ctx context.Context) { decA++ }))
	b.Register(funcTask(func(ctx context.Context) { decB++ }))

	a.Cycle(ctx) // a acquires and decides
	if decA != 1 || decB != 0 {
		t.Fatalf("after a.Cycle: decA=%d decB=%d", decA, decB)
	}
	// a's host dies (missed renewals); b takes over.
	cl.Advance(LeaseStaleAfter + time.Second)
	b.Cycle(ctx)
	if decB != 1 {
		t.Fatalf("b should decide after takeover: decB=%d", decB)
	}
	// a comes back: it must NOT decide (fresh foreign holder).
	a.Cycle(ctx)
	if decA != 1 {
		t.Fatalf("a decided after losing the lease: decA=%d", decA)
	}
	// b keeps deciding while renewing.
	cl.Advance(LeaseRenewInterval + time.Second)
	b.Cycle(ctx)
	if decB != 2 {
		t.Fatalf("b should keep deciding: decB=%d", decB)
	}
}

func TestNoLeaderEvent(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	fake.SetNode("n1", map[string]string{
		"simplek8s.org/reboot-state": `{"state":"draining","since":"2026-09-05T12:00:00Z"}`,
	}, false, nil, "uid-n1")
	cl := newClock()
	ctx := context.Background()

	a := newTestEngine(t, fake, cl, "pod-a/uid-a")
	a.Cycle(ctx) // becomes leader: no no-leader event
	if countReason(fake, "NoValidLeader") != 0 {
		t.Fatal("no-leader event while a valid leader exists")
	}

	// The lease goes stale and a cannot even read the API: no valid
	// leader while reboot state exists -> rate-limited event.
	cl.Advance(LeaseStaleAfter + time.Second)
	fake.InjectFailure("/leases/"+LeaseName, 6)
	a.Cycle(ctx)
	if n := countReason(fake, "NoValidLeader"); n != 1 {
		t.Fatalf("NoValidLeader events = %d, want 1", n)
	}
	// Rate-limited: another bad cycle adds nothing.
	fake.InjectFailure("/leases/"+LeaseName, 6)
	a.Cycle(ctx)
	if n := countReason(fake, "NoValidLeader"); n != 1 {
		t.Fatalf("NoValidLeader events = %d, want 1 (rate-limited)", n)
	}
}

func TestReadyzCycle(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	fake.SetNode("n1", nil, false, nil, "uid-n1")
	cl := newClock()
	ctx := context.Background()

	a := newTestEngine(t, fake, cl, "pod-a/uid-a")
	if !a.LastSuccessfulCycle().IsZero() {
		t.Fatal("LastSuccessfulCycle should start zero")
	}
	a.Cycle(ctx)
	first := a.LastSuccessfulCycle()
	if first.IsZero() {
		t.Fatal("cycle should record success")
	}
	// API unreachable: the cycle does not count as successful.
	fake.InjectFailure("/api/v1/nodes", 6)
	cl.Advance(time.Second)
	a.Cycle(ctx)
	if got := a.LastSuccessfulCycle(); !got.Equal(first) {
		t.Fatalf("LastSuccessfulCycle moved to %v after a failed cycle", got)
	}
}

func TestFeatureConfigLoad(t *testing.T) {
	fake := kubetest.NewFakeAPI()
	defer fake.Close()
	fake.SetNode("n1", nil, false, nil, "uid-n1")
	cl := newClock()
	ctx := context.Background()
	const cmNS, cmName = "default", "simplek8s-controller"

	a := newTestEngine(t, fake, cl, "pod-a/uid-a")

	// No ConfigMap yet: built-in defaults.
	a.Cycle(ctx)
	if got := a.FeatureConfig(); !reflect.DeepEqual(got, config.Defaults()) {
		t.Fatalf("absent ConfigMap: got %+v, want defaults", got)
	}

	// Present ConfigMap: values apply on top of the last snapshot.
	fake.SetConfigMap(cmNS, cmName, map[string]string{
		"engine.interval":        "30s",
		"reboots.max-concurrent": "4",
	})
	a.Cycle(ctx)
	got := a.FeatureConfig()
	if got.EngineInterval != 30*time.Second || got.MaxConcurrentReboots != 4 {
		t.Fatalf("ConfigMap values not applied: %+v", got)
	}
	if got.RebootDrainTimeout != 10*time.Minute {
		t.Fatalf("absent key must keep default: %+v", got)
	}

	// Invalid value: last valid is kept.
	fake.SetConfigMap(cmNS, cmName, map[string]string{
		"reboots.max-concurrent": "nope",
	})
	a.Cycle(ctx)
	if got := a.FeatureConfig(); got.MaxConcurrentReboots != 4 {
		t.Fatalf("invalid value must keep last valid: %+v", got)
	}

	// ConfigMap removed: back to built-in defaults.
	fake.RemoveConfigMap(cmNS, cmName)
	a.Cycle(ctx)
	if got := a.FeatureConfig(); !reflect.DeepEqual(got, config.Defaults()) {
		t.Fatalf("removed ConfigMap: got %+v, want defaults", got)
	}
}

type funcTask func(ctx context.Context)

func (f funcTask) Run(ctx context.Context) { f(ctx) }

func countReason(fake *kubetest.FakeAPI, reason string) int {
	n := 0
	for _, e := range fake.Events() {
		if e["reason"] == reason {
			n++
		}
	}
	return n
}
