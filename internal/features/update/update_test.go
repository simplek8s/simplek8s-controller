package update

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeStore struct {
	mu      sync.Mutex
	vers    []string
	staged  []StageRequest
	err     error
	flavors map[string]string
	// defGoal records the last EnsureBootGoal target (the fake's
	// DEFAULT).
	defGoal string
}

func (s *fakeStore) Versions(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.vers...), nil
}

func (s *fakeStore) Stage(ctx context.Context, req StageRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staged = append(s.staged, req)
	if s.err == nil {
		for _, v := range s.vers {
			if v == req.Version {
				return nil
			}
		}
		s.vers = append(s.vers, req.Version)
	}
	return s.err
}

func (s *fakeStore) setFlavor(ts, flavor string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flavors[ts] = flavor
}

func (s *fakeStore) set(vers ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vers = vers
	if s.flavors == nil {
		s.flavors = map[string]string{}
	}
}

func (s *fakeStore) setStageErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeStore) stagedVersions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.staged))
	for i, r := range s.staged {
		out[i] = r.Version
	}
	return out
}

// countingRepo serves a re-signable release index and counts index hits.
type countingRepo struct {
	key       *testKey
	srv       *httptest.Server
	mu        sync.Mutex
	index     []byte
	sig       []byte
	indexHits int
}

func newCountingRepo(t *testing.T, key *testKey) *countingRepo {
	t.Helper()
	r := &countingRepo{key: key}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch req.URL.Path {
		case "/SHA256SUMS":
			r.indexHits++
			w.Write(r.index)
		case "/SHA256SUMS.gpg":
			w.Write(r.sig)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *countingRepo) URL() string { return r.srv.URL }

// set replaces the served index, re-signing it with the repo key.
func (r *countingRepo) set(t *testing.T, files map[string][]byte) {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var index bytes.Buffer
	for _, n := range names {
		index.WriteString(sha256line(files[n], n) + "\n")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.index = index.Bytes()
	r.sig = r.key.sign(t, r.index, true)
}

func (r *countingRepo) hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.indexHits
}

type updateHarness struct {
	t      *testing.T
	fake   *kubetest.FakeAPI
	eng    *engine.Engine
	cl     *clock
	f      *Feature
	store  *fakeStore
	repo   *countingRepo
	key    *testKey
	logBuf *bytes.Buffer
}

func newUpdateHarness(t *testing.T, mode string, kernelVersion string, anns map[string]string, storeVers []string) *updateHarness {
	return newUpdateHarnessKey(t, mode, kernelVersion, anns, storeVers, newTestKey(t, true))
}

// newUpdateHarnessKey is the harness with an explicit signing key (used
// to build second repos sharing the pod's keyring).
func newUpdateHarnessKey(t *testing.T, mode string, kernelVersion string, anns map[string]string, storeVers []string, key *testKey) *updateHarness {
	t.Helper()
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	fake.SetNode("w1", anns, false, nil, "uid-w1")
	fake.SetNodeInfo("w1", kernelVersion, "amd64")

	cl := &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	repo := newCountingRepo(t, key)
	repo.set(t, kernelIndex())
	store := &fakeStore{}
	store.set(storeVers...)

	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	cm := map[string]string{
		"updates.update-mode": mode,
		"updates.url":         repo.URL(),
		"updates.windows":     `["@every 2m"]`,
	}
	fake.SetConfigMap("default", "simplek8s-controller", cm)

	eng, err := engine.New(engine.Config{
		CredsDir:           creds.Dir,
		APIEndpoint:        fake.Server.URL,
		Identity:           "pod-x/uid-x",
		NodeName:           "w1",
		LeaseNamespace:     "default",
		LeaseName:          engine.LeaseName,
		ConfigMapNamespace: "default",
		ConfigMapName:      "simplek8s-controller",
		Now:                cl.Now,
		Sleep:              func(ctx context.Context, d time.Duration) error { return nil },
		Log:                log,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &updateHarness{
		t: t, fake: fake, eng: eng, cl: cl, store: store, repo: repo,
		key: key, logBuf: logBuf,
	}
	h.f = New(eng, Config{
		NodeName:        "w1",
		EventNamespace:  "default",
		Features:        eng.FeatureConfig,
		EmbeddedKeyring: key.keyring,
		CustomKeyring:   filepath.Join(t.TempDir(), "absent-custom.gpg"),
		Store:           store,
		Now:             cl.Now,
		Log:             log,
	})
	return h
}

// tick runs one engine cycle (config reload + all local tasks).
func (h *updateHarness) tick() { h.eng.Cycle(context.Background()) }

func (h *updateHarness) nextKernel() string {
	return h.fake.NodeAnnotation("w1", "simplek8s.org/next-kernel")
}

func (h *updateHarness) rebootEligible() string {
	return h.fake.NodeAnnotation("w1", nodestate.AnnRebootEligible)
}

func (h *updateHarness) eventCount(reason string) int {
	n := 0
	for _, e := range h.fake.Events() {
		if e["reason"] == reason {
			n++
		}
	}
	return n
}

func (h *updateHarness) logs() string { return h.logBuf.String() }

// --- Bootstrap (PLAN-M2 3.5) ------------------------------------------------

func TestBootstrapAnchorsRunning(t *testing.T) {
	h := newUpdateHarness(t, "off", "6.18.48-simplek8s-202601010000 (amd64)", nil,
		[]string{"202501010000", "202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q, want running version", got)
	}
	// Idempotent: a second cycle changes nothing.
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel after second tick = %q", got)
	}
}

func TestBootstrapAnchorsNewestLocal(t *testing.T) {
	// Node does not run a simplek8s kernel: anchor the newest local.
	h := newUpdateHarness(t, "off", "6.18.48 (amd64)", nil,
		[]string{"202501010000", "202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q, want newest local", got)
	}
}

func TestBootstrapRetriesWhenNoLocalVersions(t *testing.T) {
	h := newUpdateHarness(t, "off", "6.18.48-simplek8s-202601010000 (amd64)", nil, nil)
	h.tick()
	if got := h.nextKernel(); got != "" {
		t.Fatalf("next-kernel = %q, want empty (nothing local)", got)
	}
	h.store.set("202601010000")
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q after versions appeared", got)
	}
}

func TestBootstrapNeverOverwritesOperatorPin(t *testing.T) {
	h := newUpdateHarness(t, "off", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202609090909"},
		[]string{"202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202609090909" {
		t.Fatalf("operator pin clobbered: %q", got)
	}
}

// --- Off mode -----------------------------------------------------------------

func TestOffModeIsInert(t *testing.T) {
	h := newUpdateHarness(t, "off", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if h.repo.hits() != 0 {
		t.Fatalf("off mode must not check the repo: %d hits", h.repo.hits())
	}
	if h.eventCount("UpdateAvailable") != 0 {
		t.Fatal("off mode must not fire UpdateAvailable")
	}
	if h.eventCount("UpdateCheckError") != 0 {
		t.Fatal("off mode must not fire UpdateCheckError")
	}
}

// --- Check + events (PLAN-M2 3.6/3.11) ---------------------------------------

func TestUpdateAvailableOncePerVersion(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if got := h.eventCount("UpdateAvailable"); got != 1 {
		t.Fatalf("UpdateAvailable = %d, want 1", got)
	}
	if h.repo.hits() != 1 {
		t.Fatalf("repo hits = %d, want 1", h.repo.hits())
	}

	// Within the check interval: no re-check, no repeat event.
	h.tick()
	if got := h.eventCount("UpdateAvailable"); got != 1 {
		t.Fatalf("UpdateAvailable after 2nd tick = %d, want 1", got)
	}
	if h.repo.hits() != 1 {
		t.Fatalf("repo hits after 2nd tick = %d, want 1", h.repo.hits())
	}

	// Past the occurrence: re-check, but the same version does not
	// re-fire the event.
	h.cl.Advance(time.Hour + time.Second)
	h.tick()
	if h.repo.hits() != 2 {
		t.Fatalf("repo hits after interval = %d, want 2", h.repo.hits())
	}
	if got := h.eventCount("UpdateAvailable"); got != 1 {
		t.Fatalf("UpdateAvailable after re-check = %d, want 1 (once per version)", got)
	}
}

func TestNoEventWhenUpToDate(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202608291203 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		[]string{"202608291203"})
	h.tick()
	if h.eventCount("UpdateAvailable") != 0 {
		t.Fatal("no event expected when already on latest")
	}
	if h.eventCount("UpdateCheckError") != 0 {
		t.Fatal("no error event expected on a successful check")
	}
}

func TestURLPrecedenceAnnotationOverConfig(t *testing.T) {
	// Both repos are signed by the pod's key (the keyring is per-pod,
	// not per-repo), so both would verify. The per-repo hit counters
	// reveal which URL was actually used.
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)", nil,
		[]string{"202601010000"})
	repo2 := newCountingRepo(t, h.key)
	repo2.set(t, kernelIndex())
	// Anchor + point the per-node override at repo2.
	h.fake.SetNode("w1", map[string]string{
		"simplek8s.org/next-kernel": "202601010000",
		"simplek8s.org/update-url":  repo2.URL(),
	}, false, nil, "uid-w1")
	h.fake.SetNodeInfo("w1", "6.18.48-simplek8s-202601010000 (amd64)", "amd64")

	h.tick()
	if repo2.hits() != 1 {
		t.Fatalf("annotation repo hits = %d, want 1", repo2.hits())
	}
	if h.repo.hits() != 0 {
		t.Fatalf("config repo hits = %d, want 0 (annotation must win)", h.repo.hits())
	}
	if h.eventCount("UpdateAvailable") != 1 {
		t.Fatalf("UpdateAvailable = %d, want 1", h.eventCount("UpdateAvailable"))
	}
}

func TestCheckErrorRateLimitedAndRecovers(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	// Serve an index signed by a FORKEY: verification must fail.
	forger := newTestKey(t, true)
	forge := newCountingRepo(t, forger)
	forge.set(t, kernelIndex())
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.update-mode": "full",
		"updates.url":         forge.URL(),
		"updates.windows":     `["@every 2m"]`,
	})

	h.tick() // check fails
	if got := h.eventCount("UpdateCheckError"); got != 1 {
		t.Fatalf("UpdateCheckError = %d, want 1", got)
	}
	// Still failing in a later occurrence: rate-limited, no new event.
	h.cl.Advance(time.Hour + time.Second)
	h.tick()
	if got := h.eventCount("UpdateCheckError"); got != 1 {
		t.Fatalf("UpdateCheckError after repeat failure = %d, want 1", got)
	}

	// Recovery: the good repo is restored, the failing stretch ends.
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.update-mode": "full",
		"updates.url":         h.repo.URL(),
		"updates.windows":     `["@every 2m"]`,
	})
	h.cl.Advance(time.Hour + time.Second)
	h.tick()
	if got := h.eventCount("UpdateAvailable"); got != 1 {
		t.Fatalf("UpdateAvailable after recovery = %d, want 1", got)
	}

	// A new failing stretch fires the event again.
	h.fake.SetConfigMap("default", "simplek8s-controller", map[string]string{
		"updates.update-mode": "full",
		"updates.url":         forge.URL(),
		"updates.windows":     `["@every 2m"]`,
	})
	h.cl.Advance(time.Hour + time.Second)
	h.tick()
	if got := h.eventCount("UpdateCheckError"); got != 2 {
		t.Fatalf("UpdateCheckError after new failing stretch = %d, want 2", got)
	}
}

// --- Staging (PLAN-M2 3.7) -------------------------------------------------

// TestStagesAvailableReleaseAndAnchors: a quiescent node with a newer
// release available (not local) gets it staged AND anchored.
func TestStagesAvailableReleaseAndAnchors(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 1 || got[0] != "202608291203" {
		t.Fatalf("staged = %v, want [202608291203]", got)
	}
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want anchored 202608291203", got)
	}
	if h.eventCount("UpdateStaged") != 1 {
		t.Fatalf("UpdateStaged = %d, want 1", h.eventCount("UpdateStaged"))
	}
}

// TestDefensiveRestageStagesMissingAnchoredVersion: the node is anchored
// to a version whose files are missing locally; it is re-staged, and the
// (already set) annotation is left untouched (anchor is gated quiescent).
func TestDefensiveRestageStagesMissingAnchoredVersion(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		[]string{"202601010000"})
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 1 || got[0] != "202608291203" {
		t.Fatalf("staged = %v, want [202608291203]", got)
	}
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("annotation must be untouched: %q", got)
	}
}

// TestStagingSkippedWhenVersionAlreadyLocal: nothing to stage.
func TestStagingSkippedWhenVersionAlreadyLocal(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		[]string{"202601010000", "202608291203"})
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 0 {
		t.Fatalf("staged = %v, want none (already local)", got)
	}
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want unchanged", got)
	}
}

// TestStagingFailureFiresEventNoAnnotationChange: a failed stage fires
// UpdateStagingSkipped and never changes the annotation.
func TestStagingFailureFiresEventNoAnnotationChange(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.store.setStageErr(errors.New("disk full"))
	h.tick()
	if got := h.nextKernel(); got != "202601010000" {
		t.Fatalf("next-kernel = %q, want unchanged after failed stage", got)
	}
	if h.eventCount("UpdateStagingSkipped") != 1 {
		t.Fatalf("UpdateStagingSkipped = %d, want 1", h.eventCount("UpdateStagingSkipped"))
	}
	if h.eventCount("UpdateStaged") != 0 {
		t.Fatalf("UpdateStaged = %d, want 0", h.eventCount("UpdateStaged"))
	}
}

// --- Reboot-eligible plan trigger (PLAN-M2 3.8) --------------------------

// TestFreshFullStageSetsNoTrigger: a genuine fresh full-mode stage
// anchors next-kernel but never sets the abolished reboot-eligible
// plan trigger (PLAN.md §3.4 decision 12 — pod-side enqueue replaces
// it).
func TestFreshFullStageSetsNoTrigger(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want anchored 202608291203", got)
	}
	if got := h.rebootEligible(); got != "" {
		t.Fatalf("reboot-eligible = %q, want absent (trigger abolished)", got)
	}
}

// TestStageModeDoesNotSetRebootEligible: staging mode stages and anchors the
// release but never sets the plan trigger (plans are a full-mode concern).
func TestStageModeDoesNotSetRebootEligible(t *testing.T) {
	h := newUpdateHarness(t, "stage", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202601010000"},
		[]string{"202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want anchored 202608291203", got)
	}
	if got := h.rebootEligible(); got != "" {
		t.Fatalf("reboot-eligible = %q, want absent in staging mode", got)
	}
}

// TestDefensiveRestageDoesNotSetRebootEligible: re-staging a version that is
// already the anchor (files missing locally) is not a fresh stage, so it does
// not set the plan trigger (a plan is the consequence of staging a version
// not in /boot before).
func TestDefensiveRestageDoesNotSetRebootEligible(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		[]string{"202601010000"})
	h.tick()
	if got := h.nextKernel(); got != "202608291203" {
		t.Fatalf("next-kernel = %q, want unchanged 202608291203", got)
	}
	if got := h.rebootEligible(); got != "" {
		t.Fatalf("reboot-eligible = %q, want absent (defensive re-stage)", got)
	}
}

// EnsureBootGoal records the goal as the fake DEFAULT when the version
// is present, or ErrGoalAbsent (nothing written) when it is not. It
// reports whether DEFAULT changed (the transition signal of decision
// 34).
func (s *fakeStore) EnsureBootGoal(ctx context.Context, version, arch string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.vers {
		if v == version {
			changed := s.defGoal != version
			s.defGoal = version
			return changed, nil
		}
	}
	return false, ErrGoalAbsent
}

// Kernels lists staged basenames with flavor parts (default x86-64
// unless setFlavor says otherwise).
func (s *fakeStore) Kernels(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flavors == nil {
		s.flavors = map[string]string{}
	}
	out := make([]string, 0, len(s.vers))
	for _, v := range s.vers {
		flavor, ok := s.flavors[v]
		if !ok {
			flavor = "x86-64"
		}
		out = append(out, kernelStoredName(v, flavor))
	}
	return out, nil
}
