// Command simplek8s-controller is a DaemonSet pod: the per-node reboot
// executor plus the shared (leader-elected) orchestrator and HTTP API.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/api"
	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/features/reboot"
	"github.com/simplek8s/simplek8s-controller/internal/features/update"
	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// Build information, injected at compile time via -ldflags (see Makefile
// and Dockerfile). Defaults for plain `go run`/`go build`.
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

// logLevelFromEnv maps the SIMPLEK8S_LOG_LEVEL env (debug/info/warn/error,
// case-insensitive) to a slog level for the chattier development logging;
// empty or unknown values fall back to Info.
func logLevelFromEnv(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	// Deployment wiring only. Feature configuration lives in the
	// flat-key ConfigMap (PLAN-M2 3.2); the old feature flags are
	// removed, so an old DS manifest fails at startup with
	// "flag provided but not defined" (the intended migration signal).
	var (
		listen    = flag.String("listen", ":8080", "API bind address")
		apiServer = flag.String("kube-apiserver", "", "API endpoint (default: in-cluster)")
		credsDir  = flag.String("creds-dir", "/var/run/secrets/kubernetes.io/serviceaccount", "serviceaccount creds dir")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr,
		&slog.HandlerOptions{Level: logLevelFromEnv(os.Getenv("SIMPLEK8S_LOG_LEVEL"))}))

	// Optional API token: with the Secret present every API
	// endpoint but the probes requires its bearer; without it the API
	// serves loopback clients only (kubectl port-forward), enforced in
	// the auth middleware — never accidentally open to the cluster.
	token := os.Getenv("SIMPLEK8S_API_TOKEN")
	if token == "" {
		log.Warn("SIMPLEK8S_API_TOKEN is empty; API accepts loopback clients (kubectl port-forward) only")
	}
	nodeName := os.Getenv("NODE_NAME")
	podName := os.Getenv("POD_NAME")
	podUID := os.Getenv("POD_UID")
	podNS := os.Getenv("POD_NAMESPACE")
	if podNS == "" {
		podNS = "default"
	}
	if nodeName == "" || podName == "" || podUID == "" {
		log.Error("NODE_NAME, POD_NAME and POD_UID environment variables are required (downward API)")
		os.Exit(1)
	}
	log.Info("starting simplek8s-controller",
		"version", version, "commit", commit, "built", builtAt,
		"node", nodeName, "pod", podName, "podUID", podUID, "namespace", podNS,
		"listen", *listen)
	// The leader Lease lives in the controller's own namespace. Node
	// Events go to the "default" namespace: the API server rejects v1
	// Events whose namespaced subject disagrees with the event's
	// namespace, and cluster-scoped subjects (Node) must live in
	// "default" (where kubelet's own node events are).

	e, err := engine.New(engine.Config{
		LeaseNamespace:     podNS,
		LeaseName:          "simplek8s-controller-leader",
		Identity:           podName + "/" + podUID,
		NodeName:           nodeName,
		EventNamespace:     "default",
		ConfigMapNamespace: podNS,
		ConfigMapName:      "simplek8s-controller",
		CredsDir:           *credsDir,
		APIEndpoint:        *apiServer,
		Log:                log,
	})
	if err != nil {
		log.Error("building engine", "err", err)
		os.Exit(1)
	}

	reboot.New(e, reboot.Config{
		NodeName:       nodeName,
		PodUID:         podUID,
		EventNamespace: "default",
		Features:       e.FeatureConfig,
		Log:            log,
	})

	// Distro updates (PLAN-M2): release check (verified index), events,
	// bootstrap, defensive re-staging and physical staging. The physical
	// boot store discovers the boot device over the host /dev and mounts
	// it (M3); without a labeled device it skips the node + events.
	update.New(e, update.Config{
		NodeName:       nodeName,
		EventNamespace: "default",
		Features:       e.FeatureConfig,
		Store:          updatecore.NewPhysicalStore(updatecore.PhysicalStoreConfig{Log: log}),
		// Leftover M2 plan-state cleanup (PLAN.md §3.4 decision 15):
		// the leader deletes a stale simplek8s-update-plans
		// ConfigMap once per leadership acquisition.
		PlanConfigMapNamespace: podNS,
		PlanConfigMapName:      "simplek8s-update-plans",
		Log:                    log,
	})

	srv := api.New(api.Config{
		Kube:         e.Kube(),
		PodNamespace: podNS,
		PodSelector:  map[string]string{"app": "simplek8s-controller"},
		Features:     e.FeatureConfig,
		Log:          log,
		Ready: func() bool {
			last := e.LastSuccessfulCycle()
			return !last.IsZero() && time.Since(last) < 30*time.Second
		},
	}, token)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()
	httpErr := make(chan error, 1)
	go func() { httpErr <- httpSrv.ListenAndServe() }()

	engineDone := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(engineDone)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		<-engineDone
	case err := <-httpErr:
		if err != http.ErrServerClosed {
			log.Error("http server failed", "err", err)
			stop()
			<-engineDone
			os.Exit(1)
		}
	}
}
