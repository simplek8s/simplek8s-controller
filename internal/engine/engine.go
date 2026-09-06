package engine

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// Config carries the engine-wide settings (PLAN 3.8).
type Config struct {
	// PollInterval is the engine cycle period (default 2 s).
	PollInterval time.Duration
	// LeaseNamespace/LeaseName for the global leader lease.
	LeaseNamespace string
	LeaseName      string
	// Identity is "<pod-name>/<pod-uid>".
	Identity string
	// NodeName is the node this pod is bound to.
	NodeName string
	// EventNamespace is where controller Events are recorded. Node
	// Events are cluster-scoped subjects and must live in "default"
	// (PLAN 3.11).
	EventNamespace string
	// CredsDir/APIEndpoint for the kube client.
	CredsDir    string
	APIEndpoint string
	Log         *slog.Logger
	Now         func() time.Time
	// Reboot feature settings.
	MaxConcurrentReboots int
	OnRebootFailure      string // "pause" | "continue"
	DrainTimeout         time.Duration
	IssueGrace           time.Duration
	// RebootCmd is the command issued into the host PID namespace.
	RebootCmd []string
	// BootIDFunc returns the current host boot ID (injectable).
	BootIDFunc func() (string, error)
	// Sleep is injectable for tests (default time.Sleep via context).
	Sleep func(ctx context.Context, d time.Duration) error
}

// OrchestratorTask runs once per successful cycle, on the leader only.
type OrchestratorTask interface {
	Run(ctx context.Context)
}

// LocalFunc runs once per successful cycle, on every pod (executor role).
type LocalFunc func(ctx context.Context)

// Engine is the polling loop that ties leader election, orchestrator
// tasks and local executor functions together (PLAN 3.1).
type Engine struct {
	cfg  Config
	kube *kube.Client
	leas *Leaser

	mu       sync.Mutex
	tasks    []OrchestratorTask
	locals   []LocalFunc
	lastOk   atomic.Int64 // unixnano of last successful cycle
	apiUp    bool
	noLeader bool // last reported no-valid-leader condition
	prevLease LeaseState
	havePrev bool
}

// New builds an Engine. It fails if the kube client cannot be built.
func New(cfg Config) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	c, err := kube.New(kube.Config{
		Endpoint: cfg.APIEndpoint,
		CredsDir: cfg.CredsDir,
		Now:      cfg.Now,
		Sleep:    cfg.Sleep,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg:  cfg,
		kube: c,
		leas: NewLeaser(c, cfg.LeaseNamespace, cfg.LeaseName, cfg.Identity, cfg.Now),
	}, nil
}

// Kube exposes the shared client to feature packages.
func (e *Engine) Kube() *kube.Client { return e.kube }

// Leaser exposes the lease (for features that must VerifyOwnership).
func (e *Engine) Leaser() *Leaser { return e.leas }

// Register adds an orchestrator task (leader-only cycle work).
func (e *Engine) Register(t OrchestratorTask) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, t)
}

// RegisterLocal adds a local executor function (runs on every pod).
func (e *Engine) RegisterLocal(f LocalFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.locals = append(e.locals, f)
}

// LastSuccessfulCycle returns the time of the last cycle that completed
// successfully (0 = never). Used by /readyz (PLAN 3.14).
func (e *Engine) LastSuccessfulCycle() time.Time {
	if v := e.lastOk.Load(); v != 0 {
		return time.Unix(0, v)
	}
	return time.Time{}
}

// Run starts the polling loop and blocks until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	e.Cycle(ctx)
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Cycle(ctx)
		}
	}
}

// Cycle is one engine iteration. A cycle succeeds only when the node
// list was fetched (API reachable); the lease and all tasks run after.
func (e *Engine) Cycle(ctx context.Context) {
	nodes, err := e.kube.ListNodes(ctx)
	if err != nil {
		if e.apiUp {
			e.apiUp = false
			e.cfg.Log.Error("API server unreachable", "err", err)
		}
		return
	}
	e.apiUp = true
	now := e.cfg.Now()

	state := e.leas.Sync(ctx)
	e.logLeaseTransition(state)
	if state == LeaseNoValidLeader && anyRebootState(nodes) {
		if !e.noLeader {
			e.noLeader = true
			e.event("NoValidLeader", "no valid leader observed while reboot state exists", true)
		}
	} else {
		e.noLeader = false
	}

	if state == LeaseLeader && e.leas.CanDecide() {
		e.mu.Lock()
		tasks := append([]OrchestratorTask(nil), e.tasks...)
		e.mu.Unlock()
		for _, task := range tasks {
			task.Run(ctx)
		}
	}

	e.mu.Lock()
	locals := append([]LocalFunc(nil), e.locals...)
	e.mu.Unlock()
	for _, f := range locals {
		f(ctx)
	}

	e.lastOk.Store(now.UnixNano())
}

// logLeaseTransition logs the first observed state and every subsequent
// state change (steady state stays quiet).
func (e *Engine) logLeaseTransition(state LeaseState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.prevLease
	first := !e.havePrev
	e.prevLease = state
	e.havePrev = true
	if !first && prev == state {
		return
	}
	switch {
	case state == LeaseLeader:
		e.cfg.Log.Info("acquired leadership",
			"lease", e.cfg.LeaseName, "namespace", e.cfg.LeaseNamespace, "identity", e.cfg.Identity)
	case state == LeaseNoValidLeader:
		e.cfg.Log.Warn("no valid leader; this pod will not make decisions",
			"lease", e.cfg.LeaseName, "namespace", e.cfg.LeaseNamespace)
	case state == LeaseNotLeader:
		switch {
		case first:
			e.cfg.Log.Info("standby: another pod holds leadership",
				"lease", e.cfg.LeaseName, "namespace", e.cfg.LeaseNamespace)
		case prev == LeaseLeader:
			e.cfg.Log.Warn("lost leadership",
				"lease", e.cfg.LeaseName, "namespace", e.cfg.LeaseNamespace)
		}
	}
}

func anyRebootState(nodes []kube.Node) bool {
	for _, n := range nodes {
		if s, ok := n.Metadata.Annotations["simplek8s.org/reboot-state"]; ok && s != "" {
			return true
		}
	}
	return false
}

func (e *Engine) event(reason, message string, warning bool) {
	ev := &kube.Event{}
	ev.Metadata.Namespace = e.cfg.EventNamespace
	ev.Metadata.GenerateName = "simplek8s-"
	ev.InvolvedObject.Kind = "Pod"
	ev.InvolvedObject.Name = e.cfg.Identity
	ev.Reason = reason
	ev.Message = message
	ev.Type = "Normal"
	if warning {
		ev.Type = "Warning"
	}
	if err := e.kube.CreateEvent(context.WithoutCancel(context.Background()), e.cfg.EventNamespace, ev); err != nil {
		e.cfg.Log.Debug("event creation failed", "reason", reason, "err", err)
	}
}
