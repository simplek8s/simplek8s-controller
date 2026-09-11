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
)

// BootStore lists the release versions present on this node's boot
// partition (<boot>/simplek8s/) and stages a release onto it. The
// physical store does device discovery + mount; tests use fakes.
type BootStore interface {
	Versions(ctx context.Context) ([]string, error)
	Stage(ctx context.Context, req StageRequest) error
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
	// PlanConfigMapNamespace/PlanConfigMapName is the leader-owned plan
	// state ConfigMap (default "simplek8s-update-plans"; PLAN-M2 3.8/3.9).
	PlanConfigMapNamespace string
	PlanConfigMapName      string
	Now                    func() time.Time
	Log                    *slog.Logger
}

// Feature is the distro-update feature: the per-node release check,
// staging, and (M4) the leader-side all-or-nothing reboot plan.
type Feature struct {
	kube  *kube.Client
	leas  *engine.Leaser
	plans *plansStore
	cfg   Config
	log   *slog.Logger
	http  *http.Client

	mu         sync.Mutex
	notified   map[string]bool // rate-limited event flags
	availSeen  map[string]bool // versions for which UpdateAvailable fired
	skipLogged bool            // master-switch skip already logged at Info (edge-triggered rate limit)
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
		cfg.EmbeddedKeyring = EmbeddedKeyringPath
	}
	if cfg.CustomKeyring == "" {
		cfg.CustomKeyring = CustomKeyringPath
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if cfg.PlanConfigMapName == "" {
		cfg.PlanConfigMapName = "simplek8s-update-plans"
	}
	f := &Feature{
		kube:      e.Kube(),
		leas:      e.Leaser(),
		plans:     newPlansStore(e.Kube(), cfg.PlanConfigMapNamespace, cfg.PlanConfigMapName),
		cfg:       cfg,
		log:       cfg.Log,
		http:      cfg.HTTPClient,
		notified:  map[string]bool{},
		availSeen: map[string]bool{},
	}
	e.Register(f)               // leader-only: the reboot plan (M4)
	e.RegisterLocal(f.RunLocal) // every pod: check + stage (M2/M3)
	return f
}

// RunLocal implements engine.LocalFunc (every pod, own node).
func (f *Feature) RunLocal(ctx context.Context) {
	node, err := f.kube.GetNode(ctx, f.cfg.NodeName)
	if err != nil {
		f.log.Debug("update: own node not found", "err", err)
		return
	}
	fc := f.cfg.Features()

	f.bootstrap(ctx, node)

	if fc.UpdateMode == "off" {
		return
	}

	// Master switch (PLAN.md §3.4): update work runs only while a
	// window is open. Closed (or empty) windows skip the cycle with no
	// HTTP fetch and no partition mount.
	now := f.cfg.Now()
	if !cron.WindowsOpen(fc.UpdateWindows, fc.UpdateWindowGrace, now) {
		f.logWindowSkip()
		return
	}

	// One check per occurrence (PLAN.md §3.4, decision 21): the newest
	// occurrence at or before now, claimed with a conditional RMW
	// BEFORE fetching so pod restarts never re-fetch a claimed
	// occurrence. The claim persists in the update-last-check
	// annotation; an unparseable stored value reads as absent.
	occ, ok := cron.NewestOccurrence(fc.UpdateWindows, now)
	if !ok {
		return
	}
	if !f.claimCheck(ctx, node, occ) {
		return
	}
	f.mu.Lock()
	f.skipLogged = false
	f.mu.Unlock()

	// The check runs first: staging needs the verified index (checksums).
	res, err := f.doCheck(ctx, node, fc)
	if err != nil {
		return // check failure: no state change (event already fired)
	}
	f.maybeStage(ctx, node, fc, res)
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
func (f *Feature) bootstrap(ctx context.Context, node *kube.Node) {
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	if ui.NextKernelPresent {
		return
	}
	if f.cfg.Store == nil {
		return
	}
	local, err := f.cfg.Store.Versions(ctx)
	if err != nil {
		f.log.Debug("update: local version scan failed", "err", err)
		return
	}
	if len(local) == 0 {
		return // case 3: retry next cycle
	}
	running := RunningVersion(node.Status.NodeInfo.KernelVersion)
	target := ""
	for _, v := range local {
		if v == running {
			target = v
			break
		}
	}
	if target == "" {
		for _, v := range local {
			if target == "" || NewerTS(v, target) {
				target = v
			}
		}
	}
	if err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3,
		nodestate.NextKernelBuild(target, nodestate.PrecondAbsent)); err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: bootstrap anchor failed", "version", target, "err", err)
		}
		return
	}
	f.log.Info("update: bootstrapped next-kernel", "version", target)
}

// --- Staging (PLAN-M2 3.7) --------------------------------------------

// maybeStage ensures the versions this node needs are on its boot
// partition, using the verified index from a successful check. It (1)
// defensively re-stages the anchored version if its files are missing
// (no annotation change: the anchor already holds it), and (2) stages the
// newest available release and, on success, anchors next-kernel := V.
func (f *Feature) maybeStage(ctx context.Context, node *kube.Node, fc config.Config, res CheckResult) {
	if f.cfg.Store == nil || res.Arch == "" {
		return
	}
	local, err := f.cfg.Store.Versions(ctx)
	f.mu.Lock()
	if err != nil {
		f.noteScanErrorLocked(f.cfg.NodeName, true, err)
	} else {
		f.noteScanErrorLocked(f.cfg.NodeName, false, nil)
	}
	f.mu.Unlock()
	if err != nil {
		return
	}
	localSet := make(map[string]bool, len(local))
	for _, v := range local {
		localSet[v] = true
	}
	running := RunningVersion(node.Status.NodeInfo.KernelVersion)
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)

	// freshStage is the genuine new-version stage (target 2 below): a
	// release not already local and not already the anchor. Only this —
	// never the defensive re-stage of an already-anchored version — sets
	// the reboot-eligible plan trigger (PLAN-M2 3.8: a plan is the
	// consequence of staging a version not in /boot before).
	freshStage := res.Available && res.Latest != "" && res.Latest != running &&
		!localSet[res.Latest] && ui.NextKernel != res.Latest

	var targets []string
	// (1) anchored version missing locally (defensive; never the anchor value).
	if ui.NextKernelPresent && ui.NextKernelParseErr == "" &&
		ui.NextKernel != running && !localSet[ui.NextKernel] {
		targets = append(targets, ui.NextKernel)
	}
	// (2) newest available release not yet local.
	if res.Available && res.Latest != "" && res.Latest != running &&
		!localSet[res.Latest] && ui.NextKernel != res.Latest {
		targets = append(targets, res.Latest)
	}
	for _, v := range targets {
		if f.stageOne(ctx, node, fc, res, v) && v == res.Latest {
			f.anchor(ctx, node, res.Latest, running, fc.UpdateMode == "full" && freshStage)
		}
	}
}

// stageOne stages one release ts onto the local boot partition. It
// returns true on success; a failure fires a rate-limited
// UpdateStagingSkipped (no annotation change, no cluster impact).
func (f *Feature) stageOne(ctx context.Context, node *kube.Node, fc config.Config, res CheckResult, v string) bool {
	artifact := kernelArtifactName(v, res.Arch)
	checksum, ok := res.Sums[artifact]
	if !ok {
		f.log.Warn("update: no verified checksum for version; not staging",
			"version", v, "artifact", artifact)
		return false
	}
	req := StageRequest{
		Version:         v,
		Arch:            res.Arch,
		Checksum:        checksum,
		ArtifactFile:    artifact,
		RepoBase:        res.URL,
		Preserve:        fc.UpdatePreserve,
		MaxPercentUsage: fc.UpdateMaxPercentUsage,
		Running:         RunningVersion(node.Status.NodeInfo.KernelVersion),
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
// When eligible is true (a fresh full-mode stage) it also sets the
// reboot-eligible plan trigger in the same conditional patch, so the
// trigger and the anchor can never diverge (PLAN-M2 3.8).
func (f *Feature) anchor(ctx context.Context, node *kube.Node, version, running string, eligible bool) {
	var build nodestate.BuildFunc
	if eligible {
		build = nodestate.NextKernelEligibleBuild(version, nodestate.PrecondQuiescent(running))
	} else {
		build = nodestate.NextKernelBuild(version, nodestate.PrecondQuiescent(running))
	}
	if err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3, build); err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: anchor failed", "version", version, "err", err)
		}
		return
	}
	f.log.Info("update: anchored next-kernel", "version", version, "eligible", eligible)
}

// --- Check (PLAN-M2 3.6) ------------------------------------------------

// doCheck runs the verified release check and applies its events:
// UpdateAvailable (once per version) or a rate-limited UpdateCheckError.
// It returns the (possibly empty) result and the error; a failure never
// changes any state. Staging is a separate step (maybeStage) that runs
// only after a successful check, since it needs the verified index.
func (f *Feature) doCheck(ctx context.Context, node *kube.Node, fc config.Config) (CheckResult, error) {
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	repoURL := ui.UpdateURL
	if repoURL == "" {
		repoURL = fc.UpdateURL
	}
	res, err := f.Check(ctx, node, repoURL)
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
	if warning {
		ev.Type = "Warning"
	}
	if err := f.kube.CreateEvent(context.WithoutCancel(context.Background()), f.cfg.EventNamespace, ev); err != nil {
		f.log.Debug("update: event creation failed", "reason", reason, "err", err)
	}
}
