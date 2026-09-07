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
	Now        func() time.Time
	Log        *slog.Logger
}

// Feature is the distro-update feature (local executor role only in
// M2/M3; the leader-side plan logic lands in M4).
type Feature struct {
	kube *kube.Client
	cfg  Config
	log  *slog.Logger
	http *http.Client

	mu        sync.Mutex
	lastCheck map[string]time.Time // node -> last check attempt
	notified  map[string]bool      // rate-limited event flags
	availSeen map[string]bool      // versions for which UpdateAvailable fired
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
	f := &Feature{
		kube:      e.Kube(),
		cfg:       cfg,
		log:       cfg.Log,
		http:      cfg.HTTPClient,
		lastCheck: map[string]time.Time{},
		notified:  map[string]bool{},
		availSeen: map[string]bool{},
	}
	e.RegisterLocal(f.RunLocal)
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

	// Heavy local work (boot-partition mount) is throttled to the check
	// interval; a failed/skipped run retries on the next interval.
	f.mu.Lock()
	last, checked := f.lastCheck[f.cfg.NodeName]
	due := !checked || f.cfg.Now().Sub(last) >= fc.UpdateCheckInterval
	f.mu.Unlock()
	if !due {
		return
	}

	// The check runs first: staging needs the verified index (checksums).
	res, err := f.doCheck(ctx, node, fc)
	if err != nil {
		return // check failure: no state change (event already fired)
	}
	f.maybeStage(ctx, node, fc, res)
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
			f.log.Warn("update: bootstrap anchor failed", "node", node.Metadata.Name, "version", target, "err", err)
		}
		return
	}
	f.log.Info("update: bootstrapped next-kernel", "node", node.Metadata.Name, "version", target)
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
	if err != nil {
		f.log.Debug("update: local version scan failed", "err", err)
		return
	}
	localSet := make(map[string]bool, len(local))
	for _, v := range local {
		localSet[v] = true
	}
	running := RunningVersion(node.Status.NodeInfo.KernelVersion)
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)

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
			f.anchor(ctx, node, res.Latest, running)
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
			"node", node.Metadata.Name, "version", v, "artifact", artifact)
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
		f.log.Warn("update: staging skipped", "node", node.Metadata.Name, "version", v, "err", err)
		f.event(node.Metadata.Name, "UpdateStagingSkipped",
			fmt.Sprintf("staging %s skipped: %v", v, err), true)
		return false
	}
	f.event(node.Metadata.Name, "UpdateStaged",
		fmt.Sprintf("staged release %s on %s", v, node.Status.NodeInfo.Architecture), false)
	f.log.Info("update: staged release", "node", node.Metadata.Name, "version", v)
	return true
}

// anchor sets next-kernel := version when the node is quiescent (writer
// discipline, PLAN-M2 3.5): a held/re-pinned value is never clobbered.
func (f *Feature) anchor(ctx context.Context, node *kube.Node, version, running string) {
	if err := nodestate.PatchTransition(ctx, f.kube, node.Metadata.Name, 3,
		nodestate.NextKernelBuild(version, nodestate.PrecondQuiescent(running))); err != nil {
		if err != nodestate.ErrAbort {
			f.log.Warn("update: anchor failed", "node", node.Metadata.Name, "version", version, "err", err)
		}
		return
	}
	f.log.Info("update: anchored next-kernel", "node", node.Metadata.Name, "version", version)
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
	f.lastCheck[f.cfg.NodeName] = f.cfg.Now()
	failed := err != nil
	if !failed {
		if res.Available && !f.availSeen[res.Latest] {
			f.availSeen[res.Latest] = true
			f.event(node.Metadata.Name, "UpdateAvailable",
				fmt.Sprintf("release %s available (running %s, repo %s)", res.Latest, res.Running, res.URL), false)
			f.log.Info("update: new release available", "node", node.Metadata.Name,
				"latest", res.Latest, "running", res.Running, "url", res.URL)
		}
		f.noteCheckErrorLocked(node.Metadata.Name, false)
	} else {
		f.noteCheckErrorLocked(node.Metadata.Name, true)
	}
	f.mu.Unlock()
	if failed {
		f.log.Warn("update: check failed", "node", node.Metadata.Name, "url", repoURL, "err", err)
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
