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
	"syscall"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/api"
	"github.com/simplek8s/simplek8s-controller/internal/engine"
	"github.com/simplek8s/simplek8s-controller/internal/features/reboot"
)

// Build information, injected at compile time via -ldflags (see Makefile
// and Dockerfile). Defaults for plain `go run`/`go build`.
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	var (
		maxConcurrent = flag.Int("max-concurrent-reboots", 1, "in-flight reboot nodes at once")
		onFailure     = flag.String("on-reboot-failure", "pause", "queue behavior on failure: pause|continue")
		drainTimeout  = flag.Duration("reboot-drain-timeout", 10*time.Minute, "max drain duration (wall-clock)")
		issueGrace    = flag.Duration("reboot-issue-grace", 5*time.Minute, "reboot issue grace window")
		interval      = flag.Duration("engine-interval", 2*time.Second, "engine poll period")
		listen        = flag.String("listen", ":8080", "API bind address")
		apiServer     = flag.String("kube-apiserver", "", "API endpoint (default: in-cluster)")
		credsDir      = flag.String("creds-dir", "/var/run/secrets/kubernetes.io/serviceaccount", "serviceaccount creds dir")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Mandatory API token (PLAN 3.9): a deployment without a token is not
	// supported, so the API is never accidentally exposed tokenless.
	token := os.Getenv("SIMPLEK8S_API_TOKEN")
	if token == "" {
		log.Error("SIMPLEK8S_API_TOKEN is required; refusing to start")
		os.Exit(1)
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
		"maxConcurrentReboots", *maxConcurrent, "onRebootFailure", *onFailure,
		"rebootDrainTimeout", *drainTimeout, "rebootIssueGrace", *issueGrace,
		"engineInterval", *interval, "listen", *listen)
	// The leader Lease lives in the controller's own namespace. Node
	// Events go to the "default" namespace: the API server rejects v1
	// Events whose namespaced subject disagrees with the event's
	// namespace, and cluster-scoped subjects (Node) must live in
	// "default" (where kubelet's own node events are).

	e, err := engine.New(engine.Config{
		PollInterval:         *interval,
		LeaseNamespace:       podNS,
		LeaseName:            "simplek8s-controller-leader",
		Identity:             podName + "/" + podUID,
		NodeName:             nodeName,
		EventNamespace:       "default",
		CredsDir:             *credsDir,
		APIEndpoint:          *apiServer,
		Log:                  log,
		MaxConcurrentReboots: *maxConcurrent,
		OnRebootFailure:      *onFailure,
		DrainTimeout:         *drainTimeout,
		IssueGrace:           *issueGrace,
	})
	if err != nil {
		log.Error("building engine", "err", err)
		os.Exit(1)
	}

	reboot.New(e, reboot.Config{
		NodeName:             nodeName,
		PodUID:               podUID,
		EventNamespace:       "default",
		MaxConcurrentReboots: *maxConcurrent,
		OnRebootFailure:      *onFailure,
		DrainTimeout:         *drainTimeout,
		IssueGrace:           *issueGrace,
		Log:                  log,
	})

	srv := api.New(api.Config{
		Kube:           e.Kube(),
		PodNamespace:   podNS,
		PodSelector:    map[string]string{"app": "simplek8s-controller"},
		Log:            log,
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
