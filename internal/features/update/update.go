// Package update implements the distro-update feature (PLAN-M2): the
// per-node release check (verified index), next-kernel bootstrap and
// anchoring, defensive re-staging detection, and the update events.
// Physical staging (mount, download, extract, bootloader) is M3; the
// local boot store is injected so the logic is testable standalone.
package update

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/config"
	"github.com/simplek8s/simplek8s-controller/internal/cron"
	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// BootStore lists the release versions present on this node's boot
// partition (<boot>/simplek8s/) and stages a release onto it. The
// physical store does device discovery + mount; tests use fakes.
type BootStore interface {
	Versions(ctx context.Context) ([]string, error)
	Stage(ctx context.Context, req updatecore.StageRequest) error
	// Kernels lists staged kernel basenames (with flavor part), for
	// flavor resolution (PLAN-M5 §3.1). Foreign (non-matching) files
	// are omitted.
	Kernels(ctx context.Context) ([]string, error)
	// EnsureBootGoal verifies version's kernel file is present on the
	// boot partition and ensures the bootloader DEFAULT points at it —
	// mount, compare, re-point if needed, sync, unmount — in one
	// session (PLAN.md §3.4 ordering invariant, §3.7). ErrGoalAbsent
	// (nothing written) when the file is missing. It reports whether
	// it re-pointed: re-arm rides transitions only (decision 34).
	EnsureBootGoal(ctx context.Context, version, arch string) (repointed bool, err error)
}

// Config carries the update feature's settings. Feature tuning
// (updates.* keys) comes from the per-cycle feature Config snapshot
// (PLAN-M2 3.2).
type Config struct {
	// NodeName is the node this pod is bound to.
	NodeName string
	// EventNamespace for Events (node events live in "default").
	EventNamespace string
	// Features returns the current feature config snapshot.
	Features func() config.Config
	// Keyring paths (fixed defaults, injectable for tests).
	EmbeddedKeyring string
	CustomKeyring   string
	// Store lists the versions on this node's boot partition.
	Store BootStore
	// HTTPClient for release-repo fetches.
	HTTPClient *http.Client
	// PlanConfigMapNamespace/PlanConfigMapName locates a leftover
	// M2 plan ConfigMap for the self-cleaning migration delete
	// (PLAN.md §3.4 decision 15). Empty disables the cleanup.
	PlanConfigMapNamespace string
	PlanConfigMapName      string
	Now                    func() time.Time
	Log                    *slog.Logger
}

// Feature is the distro-update feature: the per-node release check,
// staging, pod-side reboot enqueue, and leader-side per-node
// verification (PLAN.md §3.4).
type Feature struct {
	kube *kube.Client
	leas *engine.Leaser
	cfg  Config
	log  *slog.Logger
	http *http.Client

	mu         sync.Mutex
	notified   map[string]bool // rate-limited event flags
	availSeen  map[string]bool // versions for which UpdateAvailable fired
	skipLogged bool            // master-switch skip already logged at Info (edge-triggered rate limit)
	// lastGoal/goalKnown is the last next-kernel value the bootloader
	// was reconciled to (§3.7 change detection, in-memory).
	lastGoal  string
	goalKnown bool
	// flavor/flavorKnown is the resolved board flavor (PLAN-M5 §3.1),
	// cached per pod lifetime. A mid-life mix is Warn-logged, not
	// adopted (mixWarned).
	flavor       string
	flavorKnown  bool
	mixWarned    bool
	flavorWarned bool
	// nonQuiescent tracks nodes last seen non-quiescent (node -> goal)
	// for the UpdateApplied transition edge (§3.4 verification).
	nonQuiescent map[string]string
	// wasLeader tracks the last locally observed leadership belief for
	// the migration-cleanup acquisition edge (decision 15). Updated on
	// every local cycle (leader or not); read on the leader path.
	// plansCleaned remembers the done cleanup for the pod lifetime;
	// startCleaned the pod-start one-shot.
	wasLeader    bool
	plansCleaned bool
	startCleaned bool
}

// New builds the feature and registers its local task with the engine.
func New(e *engine.Engine, cfg Config) *Feature {
	if cfg.Features == nil {
		cfg.Features = func() config.Config { return config.Defaults() }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.EmbeddedKeyring == "" {
		cfg.EmbeddedKeyring = updatecore.EmbeddedKeyringPath
	}
	if cfg.CustomKeyring == "" {
		cfg.CustomKeyring = updatecore.CustomKeyringPath
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	f := &Feature{
		kube:         e.Kube(),
		leas:         e.Leaser(),
		cfg:          cfg,
		log:          cfg.Log,
		http:         cfg.HTTPClient,
		notified:     map[string]bool{},
		availSeen:    map[string]bool{},
		nonQuiescent: map[string]string{},
	}
	e.Register(f)               // leader-only: per-node verification
	e.RegisterLocal(f.RunLocal) // every pod: check + stage + enqueue
	return f
}

// RunLocal implements engine.LocalFunc (every pod, own node).
func (f *Feature) RunLocal(ctx context.Context) {
	// Leadership belief for the migration-cleanup acquisition edge,
	// plus the pod-start one-shot (decision 15, decision 26).
	f.mu.Lock()
	f.wasLeader = f.leas.Leader()
	start := !f.startCleaned
	f.startCleaned = true
	f.mu.Unlock()
	if start {
		f.cleanLeftoverEligible(ctx)
	}
	node, err := f.kube.GetNode(ctx, f.cfg.NodeName)
	if err != nil {
		f.log.Debug("update: own node not found", "err", err)
		return
	}
	fc := f.cfg.Features()
	now := f.cfg.Now()

	dirty := f.bootstrap(ctx, node)
	flavor, ok := f.flavorOf(ctx)
	if !ok {
		f.log.Debug("update: board flavor unresolved; skipping update work")
		return
	}
	if f.reconcileBootloader(ctx, node, dirty) {
		dirty = true
	}

	// Master switch (PLAN.md §3.4): update work runs only while a
	// window is open. Closed (or empty) windows skip the check/stage
	// below with no HTTP fetch and no partition mount — but never the
	// enqueue at the end (it has its own reboots-window gate).
	if cron.WindowsOpen(fc.UpdateWindows, fc.UpdateWindowGrace, now) {
		// One check per occurrence (PLAN.md §3.4, decision 21): the
		// newest occurrence at or before now, claimed with a
		// conditional RMW BEFORE fetching so pod restarts never
		// re-fetch a claimed occurrence. The claim persists in the
		// update-last-check annotation; an unparseable stored value
		// reads as absent. A covered occurrence skips the
		// check/stage only — eligibility may still need enqueueing.
		if occ, ok := cron.NewestOccurrence(fc.UpdateWindows, now); ok && f.claimCheck(ctx, node, occ) {
			f.mu.Lock()
			f.skipLogged = false
			f.mu.Unlock()

			// The check runs first: staging needs the verified
			// index (checksums).
			if res, err := f.doCheck(ctx, node, fc, flavor); err == nil {
				if f.maybeStage(ctx, node, fc, res) {
					dirty = true
				}
			} // check failure: no state change (event already fired)
		}
	} else {
		f.logWindowSkip()
	}
	if dirty {
		// Earlier steps patched the node: re-read before
		// evaluating enqueue eligibility on a fresh view.
		if fresh, err := f.kube.GetNode(ctx, f.cfg.NodeName); err == nil {
			node = fresh
		} else {
			f.log.Debug("update: own node re-read failed", "err", err)
			return
		}
	}
	// Pod-side reboot enqueue (PLAN.md §3.4, decision 31).
	f.maybeEnqueue(ctx, node, fc, now)
}

// logWindowSkip logs a skipped cycle at Info on the closed edge and at
// Debug afterwards (the PLAN.md §3.4 rate-limited log: no per-cycle
// Info spam while a window stays closed for hours).
func (f *Feature) logWindowSkip() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.skipLogged {
		f.skipLogged = true
		f.log.Info("update: window closed or empty; skipping check")
		return
	}
	f.log.Debug("update: window closed or empty; skipping check")
}

// claimCheck claims occurrence occ for this node: it returns false
// when the stored claim already covers occ (same occurrence, or a
// concurrent claim won the race), true when this pod owns the check.
func (f *Feature) claimCheck(ctx context.Context, node *kube.Node, occ time.Time) bool {
	if raw, ok := node.Metadata.Annotations[nodestate.AnnUpdateLastCheck]; ok {
		if t, err := time.Parse(time.RFC3339, raw); err == nil && !t.Before(occ) {
			return false
		}
	}
	err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3, nodestate.UpdateLastCheckBuild(occ))
	if err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: check claim failed", "occurrence", occ.UTC().Format(time.RFC3339), "err", err)
		}
		return false
	}
	return true
}

// --- Bootstrap (PLAN-M2 3.5): annotation absent, local pod -----------

// bootstrap anchors next-kernel when absent: (1) running version
// present locally -> running; (2) running not present -> newest local;
// (3) nothing local -> no annotation, retry next cycle. It runs in
// every update mode (node state, not "updates").
func (f *Feature) bootstrap(ctx context.Context, node *kube.Node) bool {
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	if ui.NextKernelPresent {
		return false
	}
	if f.cfg.Store == nil {
		return false
	}
	local, err := f.cfg.Store.Versions(ctx)
	if err != nil {
		f.log.Debug("update: local version scan failed", "err", err)
		return false
	}
	if len(local) == 0 {
		return false // case 3: retry next cycle
	}
	running := updatecore.RunningVersion(node.Status.NodeInfo.KernelVersion)
	target := ""
	for _, v := range local {
		if v == running {
			target = v
			break
		}
	}
	if target == "" {
		for _, v := range local {
			if target == "" || updatecore.NewerTS(v, target) {
				target = v
			}
		}
	}
	if err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3,
		nodestate.NextKernelBuild(target, nodestate.PrecondAbsent)); err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: bootstrap anchor failed", "version", target, "err", err)
		}
		return false
	}
	f.log.Info("update: bootstrapped next-kernel", "version", target)
	return true
}

// --- Staging (PLAN-M2 3.7) --------------------------------------------

// maybeStage ensures the versions this node needs are on its boot
// partition, using the verified index from a successful check. It (1)
// defensively re-stages the anchored version if its files are missing
// (no annotation change: the anchor already holds it) — unless the
// verified index no longer contains it, in which case the goal is
// corrected to the safe state instead (§3.10 path 2); and (2) stages
// the newest available release and, on success, anchors next-kernel :=
// V. It reports whether it wrote anything (anchor, re-arm, or
// correction: callers treat the cycle view as stale).
func (f *Feature) maybeStage(ctx context.Context, node *kube.Node, fc config.Config, res CheckResult) bool {
	if f.cfg.Store == nil || res.Arch == "" {
		return false
	}
	local, err := f.cfg.Store.Kernels(ctx)
	f.mu.Lock()
	if err != nil {
		f.noteScanErrorLocked(f.cfg.NodeName, true, err)
	} else {
		f.noteScanErrorLocked(f.cfg.NodeName, false, nil)
	}
	f.mu.Unlock()
	if err != nil {
		return false
	}
	// Own-flavor files only (PLAN-M5 §3.4): foreign flavors are never
	// staged, purged around, or treated as present.
	localSet := make(map[string]bool, len(local))
	for _, name := range local {
		if ts, fa, ok := updatecore.ParseStoredKernel(name); ok && fa == res.Arch {
			localSet[ts] = true
		}
	}
	running := updatecore.RunningVersion(node.Status.NodeInfo.KernelVersion)
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	arch := res.Arch
	dirty := false

	// (1) anchored version missing locally.
	if ui.NextKernelPresent && ui.NextKernelParseErr == "" &&
		ui.NextKernel != running && !localSet[ui.NextKernel] {
		if !indexHas(res, ui.NextKernel, arch) {
			// Positive knowledge of absence (removed from the
			// repo, or a ts that never existed): correct to the
			// safe state instead of staging (§3.10 path 2). The
			// goal changed: the fresh anchor below is evaluated
			// on the next cycle.
			return f.applySafeState(ctx, node.Metadata.Name, ui.NextKernel, running, arch)
		}
		// Defensive re-stage (never the anchor value).
		if f.stageOne(ctx, node, fc, res, ui.NextKernel) && f.rearmIfCompleted(ctx, node.Metadata.Name) {
			// Staging completed a pre-pinned version (§3.4
			// decision 14, case 2).
			dirty = true
		}
	}
	// (2) newest available release not yet local.
	if res.Available && res.Latest != "" && res.Latest != running &&
		!localSet[res.Latest] && ui.NextKernel != res.Latest {
		if f.stageOne(ctx, node, fc, res, res.Latest) {
			if f.anchor(ctx, node, res.Latest, running) {
				// A fresh-stage anchor changes the goal to a
				// well-formed present version: re-arm per the
				// normal rule (§3.4 decision 14, case 1).
				dirty = true
				f.rearmIfCompleted(ctx, node.Metadata.Name)
			} else if ui.NextKernel == res.Latest && f.rearmIfCompleted(ctx, node.Metadata.Name) {
				// Staging completed a pre-pinned version.
				dirty = true
			}
		}
	}
	return dirty
}

// indexHas reports whether the verified index contains a kernel
// artifact for ts on arch (PLAN.md §3.10 path 2: positive knowledge
// of absence vs transient staging failure).
func indexHas(res CheckResult, ts, arch string) bool {
	for file := range res.Sums {
		if v, fa, ok := updatecore.ParseKernelRelease(file); ok && v == ts && fa == arch {
			return true
		}
	}
	return false
}

// stageOne stages one release ts onto the local boot partition. It
// returns true on success; a failure fires a rate-limited
// UpdateStagingSkipped (no annotation change, no cluster impact).
func (f *Feature) stageOne(ctx context.Context, node *kube.Node, fc config.Config, res CheckResult, v string) bool {
	artifact := updatecore.ArtifactFileName(v, res.Arch)
	checksum, ok := res.Sums[artifact]
	if !ok {
		f.log.Warn("update: no verified checksum for version; not staging",
			"version", v, "artifact", artifact)
		return false
	}
	req := updatecore.StageRequest{
		Version:         v,
		Arch:            res.Arch,
		Checksum:        checksum,
		ArtifactFile:    artifact,
		RepoBase:        res.URL,
		Preserve:        fc.UpdatePreserve,
		MaxPercentUsage: fc.UpdateMaxPercentUsage,
		Running:         updatecore.RunningVersion(node.Status.NodeInfo.KernelVersion),
	}
	if err := f.cfg.Store.Stage(ctx, req); err != nil {
		f.log.Warn("update: staging skipped", "version", v, "err", err)
		f.event(node.Metadata.Name, "UpdateStagingSkipped",
			fmt.Sprintf("staging %s skipped: %v", v, err), true)
		return false
	}
	f.event(node.Metadata.Name, "UpdateStaged",
		fmt.Sprintf("staged release %s on %s", v, node.Status.NodeInfo.Architecture), false)
	f.log.Info("update: staged release", "version", v)
	return true
}

// anchor sets next-kernel := version when the node is quiescent (writer
// discipline, PLAN-M2 3.5): a held/re-pinned value is never clobbered.
// The M2 reboot-eligible plan trigger is abolished (PLAN.md §3.4
// decision 12): the anchor carries no trigger — pod-side enqueue
// replaces it. It reports whether the patch landed.
func (f *Feature) anchor(ctx context.Context, node *kube.Node, version, running string) bool {
	build := nodestate.NextKernelBuild(version, nodestate.PrecondQuiescent(running))
	if err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3, build); err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: anchor failed", "version", version, "err", err)
		}
		return false
	}
	f.log.Info("update: anchored next-kernel", "version", version)
	return true
}

// --- Check (PLAN-M2 3.6) ------------------------------------------------

// doCheck runs the verified release check and applies its events:
// UpdateAvailable (once per version) or a rate-limited UpdateCheckError.
// It returns the (possibly empty) result and the error; a failure never
// changes any state. Staging is a separate step (maybeStage) that runs
// only after a successful check, since it needs the verified index.
func (f *Feature) doCheck(ctx context.Context, node *kube.Node, fc config.Config, flavor string) (CheckResult, error) {
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	repoURL := ui.UpdateURL
	if repoURL == "" {
		repoURL = fc.UpdateURL
	}
	res, err := f.Check(ctx, node, repoURL, flavor)
	f.mu.Lock()
	failed := err != nil
	if !failed {
		if res.Available && !f.availSeen[res.Latest] {
			f.availSeen[res.Latest] = true
			f.event(node.Metadata.Name, "UpdateAvailable",
				fmt.Sprintf("release %s available (running %s, repo %s)", res.Latest, res.Running, res.URL), false)
			f.log.Info("update: new release available",
				"latest", res.Latest, "running", res.Running, "url", res.URL)
		}
		f.noteCheckErrorLocked(node.Metadata.Name, false)
	} else {
		f.noteCheckErrorLocked(node.Metadata.Name, true)
	}
	f.mu.Unlock()
	if failed {
		f.log.Warn("update: check failed", "url", repoURL, "err", err)
	}
	return res, err
}

// noteCheckErrorLocked fires UpdateCheckError on the false->true edge
// of the failing state (rate-limited: one event per failing stretch).
// Callers hold f.mu.
func (f *Feature) noteCheckErrorLocked(nodeName string, active bool) {
	key := "checkerror:" + nodeName
	if active && !f.notified[key] {
		f.notified[key] = true
		f.event(nodeName, "UpdateCheckError", "release check failed; see controller logs", true)
	}
	if !active {
		delete(f.notified, key)
	}
}

// noteScanErrorLocked logs a Warn on the false->true edge of a boot-
// partition scan failure (rate-limited: one line per failing stretch, not
// every check cycle). The node is the local one (constant per pod), so it
// is used only as the rate-limit key, not echoed in the line. Callers hold
// f.mu.
func (f *Feature) noteScanErrorLocked(nodeName string, active bool, err error) {
	key := "scanerror:" + nodeName
	if active && !f.notified[key] {
		f.notified[key] = true
		f.log.Warn("update: boot partition scan failed; not staging", "err", err)
	}
	if !active {
		delete(f.notified, key)
	}
}

// --- Events -------------------------------------------------------------

// event records a controller Event regarding the given node.
func (f *Feature) event(nodeName, reason, message string, warning bool) {
	ev := &kube.Event{}
	ev.Metadata.Namespace = f.cfg.EventNamespace
	ev.Metadata.GenerateName = "update-"
	ev.InvolvedObject.Kind = "Node"
	ev.InvolvedObject.Name = nodeName
	ev.Reason = reason
	ev.Message = message
	ev.Type = "Normal"
	// Timestamps are ours to set: the API server leaves a direct
	// CREATE untouched, and empty times render as `<unknown>`.
	now := kube.Time(time.Now().UTC())
	ev.FirstTimestamp = now
	ev.LastTimestamp = now
	ev.Count = 1
	if warning {
		ev.Type = "Warning"
	}
	if err := f.kube.CreateEvent(context.WithoutCancel(context.Background()), f.cfg.EventNamespace, ev); err != nil {
		f.log.Debug("update: event creation failed", "reason", reason, "err", err)
	}
}
