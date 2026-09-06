// Package reboot implements the v1 node-reboot feature: the queue,
// concurrency limits, PDB gating, drain, reboot issue/confirm, and the
// per-node state machine (PLAN 3.4/3.5). It registers (a) an
// orchestrator task, (b) a local executor, and (c) API routes.
package reboot

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// Config carries the reboot feature's settings (PLAN 3.8 flags).
type Config struct {
	// NodeName is the node this pod is bound to.
	NodeName string
	// PodUID is this pod's UID (env POD_UID), recorded in reboot-exec.
	PodUID string
	// EventNamespace for Events. Node Events are cluster-scoped
	// subjects and must live in "default" (PLAN 3.11).
	EventNamespace string
	// MaxConcurrentReboots is --max-concurrent-reboots (default 1).
	MaxConcurrentReboots int
	// OnRebootFailure is --on-reboot-failure: "pause" | "continue".
	OnRebootFailure string
	// DrainTimeout is --reboot-drain-timeout.
	DrainTimeout time.Duration
	// IssueGrace is --reboot-issue-grace.
	IssueGrace time.Duration
	// RebootCmd is the command run inside the host PID namespace
	// (default: nsenter -t 1 -m -u -i -n -p -- reboot).
	RebootCmd []string
	// BootIDFunc reads the host boot ID (injectable for tests).
	BootIDFunc func() (string, error)
	Now        func() time.Time
	Log        *slog.Logger
	// StartReboot runs the reboot command (injectable for tests).
	StartReboot func(ctx context.Context) error
}

// Feature is the v1 reboot feature.
type Feature struct {
	kube *kube.Client
	leas *engine.Leaser
	cfg  Config
	log  *slog.Logger

	mu            sync.Mutex
	notReadyAfter map[string]time.Time       // node -> NotReady observed after issuedAt
	denied        map[string]*denyState      // "ns/name" -> PDB-denial backoff
	notified      map[string]bool            // rate-limited event flags
	prevInflight  map[string]nodestate.State // in-flight nodes last cycle (disappearance detection)
}

type denyState struct {
	count       int
	lastAttempt time.Time
}

// New builds the feature and registers it with the engine (orchestrator
// task + local executor).
func New(e *engine.Engine, cfg Config) *Feature {
	if cfg.MaxConcurrentReboots <= 0 {
		cfg.MaxConcurrentReboots = 1
	}
	if cfg.OnRebootFailure == "" {
		cfg.OnRebootFailure = "pause"
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 5 * time.Minute
	}
	if cfg.IssueGrace <= 0 {
		cfg.IssueGrace = 120 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.BootIDFunc == nil {
		cfg.BootIDFunc = defaultBootID
	}
	if cfg.RebootCmd == nil {
		cfg.RebootCmd = []string{
			"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "reboot",
		}
	}
	if cfg.StartReboot == nil {
		cfg.StartReboot = defaultStartReboot(cfg.RebootCmd)
	}
	f := &Feature{
		kube:          e.Kube(),
		leas:          e.Leaser(),
		cfg:           cfg,
		log:           cfg.Log,
		notReadyAfter: map[string]time.Time{},
		denied:        map[string]*denyState{},
		notified:      map[string]bool{},
		prevInflight:  map[string]nodestate.State{},
	}
	e.Register(f)
	e.RegisterLocal(f.RunLocal)
	return f
}

// defaultBootID reads the kernel-global host boot ID.
func defaultBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func defaultStartReboot(cmd []string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return startCommand(ctx, cmd)
	}
}

// Run implements engine.OrchestratorTask (leader only).
func (f *Feature) Run(ctx context.Context) {
	f.runOrchestrator(ctx)
}

// RunLocal implements engine.LocalFunc (every pod, own node).
func (f *Feature) RunLocal(ctx context.Context) {
	f.runLocal(ctx)
}

// event records a controller Event regarding the given node.
func (f *Feature) event(nodeName, reason, message string, warning bool) {
	ev := &kube.Event{}
	ev.Metadata.Namespace = f.cfg.EventNamespace
	ev.Metadata.GenerateName = "reboot-"
	ev.InvolvedObject.Kind = "Node"
	ev.InvolvedObject.Name = nodeName
	ev.Reason = reason
	ev.Message = message
	ev.Type = "Normal"
	if warning {
		ev.Type = "Warning"
	}
	if err := f.kube.CreateEvent(context.WithoutCancel(context.Background()), f.cfg.EventNamespace, ev); err != nil {
		f.log.Debug("event creation failed", "reason", reason, "err", err)
	}
}

// noteEvent implements the "one Event on entry" rate limit: it fires
// only on the false->true edge of the condition.
func (f *Feature) noteEvent(key, nodeName, reason, message string, warning, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if active && !f.notified[key] {
		f.notified[key] = true
		f.event(nodeName, reason, message, warning)
	}
	if !active {
		delete(f.notified, key)
	}
}
