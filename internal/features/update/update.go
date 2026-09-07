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
// partition (<boot>/simplek8s/). M3 provides the physical store
// (device discovery + mount); tests use fakes.
type BootStore interface {
	Versions(ctx context.Context) ([]string, error)
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
	f.defensiveRestage(ctx, node, fc)

	f.mu.Lock()
	last, checked := f.lastCheck[f.cfg.NodeName]
	f.mu.Unlock()
	if !checked || f.cfg.Now().Sub(last) >= fc.UpdateCheckInterval {
		f.check(ctx, node, fc)
	}
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

// --- Defensive re-staging (PLAN-M2 3.7) --------------------------------

// defensiveRestage detects a missing locally-staged version that the
// node is anchored to and (M3) re-stages it. Files only: the
// annotation already holds the missing version, it is never touched.
func (f *Feature) defensiveRestage(ctx context.Context, node *kube.Node, fc config.Config) {
	ui := nodestate.ParseUpdate(node.Metadata.Annotations)
	if !ui.NextKernelPresent || ui.NextKernelParseErr != "" {
		return
	}
	running := RunningVersion(node.Status.NodeInfo.KernelVersion)
	if ui.NextKernel == running {
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
	for _, v := range local {
		if v == ui.NextKernel {
			return // present locally: nothing to do
		}
	}
	f.log.Warn("update: next-kernel version missing locally; re-staging (M3)",
		"node", node.Metadata.Name, "version", ui.NextKernel)
	// M3: f.cfg.Store.Stage(ctx, ui.NextKernel)
}

// --- Check (PLAN-M2 3.6) ------------------------------------------------

// check runs the verified release check when due and applies its
// consequences: UpdateAvailable (once per version) or a rate-limited
// UpdateCheckError. A check failure never changes any state.
func (f *Feature) check(ctx context.Context, node *kube.Node, fc config.Config) {
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
		if res.Available {
			if !f.availSeen[res.Latest] {
				f.availSeen[res.Latest] = true
				f.event(node.Metadata.Name, "UpdateAvailable",
					fmt.Sprintf("release %s available (running %s, repo %s)", res.Latest, res.Running, res.URL), false)
				f.log.Info("update: new release available", "node", node.Metadata.Name,
					"latest", res.Latest, "running", res.Running, "url", res.URL)
			}
			f.noteCheckErrorLocked(node.Metadata.Name, false)
			f.mu.Unlock()
			// Staging self-gates on "already local", so it is safe on
			// every check; a failed/skipped staging retries next check.
			f.stageIfDue(ctx, node, fc, res)
			return
		}
		f.noteCheckErrorLocked(node.Metadata.Name, false)
	} else {
		f.noteCheckErrorLocked(node.Metadata.Name, true)
	}
	f.mu.Unlock()
	if failed {
		f.log.Warn("update: check failed", "node", node.Metadata.Name, "url", repoURL, "err", err)
	}
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

// stageIfDue is the M3 staging seam. When a release is available and
// not already on the boot partition, M3 downloads + verifies + extracts
// it, writes the bootloader entry, purges, and anchors
// next-kernel := V (writer discipline, quiescent-at-write). In M2 it is
// a no-op: staging is physical work deferred to M3; the seam keeps the
// check flow stable so M3 only fills in the body.
func (f *Feature) stageIfDue(ctx context.Context, node *kube.Node, fc config.Config, res CheckResult) {
	// M3: f.cfg.Store.Stage(ctx, res.Latest) then anchor under PrecondQuiescent.
	f.log.Debug("update: staging is an M3 operation", "node", node.Metadata.Name, "version", res.Latest)
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
