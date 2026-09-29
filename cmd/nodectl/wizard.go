package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Web setup wizard (PLAN.md §3.16, M9): HTTPS on :5443 serving
// the embedded SPA plus a same-origin JSON API. Same
// human-output + exits discipline as §3.13, plus exitIdle (3)
// when there is nothing to manage.

// Wizard states (M9 D5): derived per request from
// simplek8s.yaml × /etc/kubernetes × admin.conf.
type wizardState string

const (
	wizLive   wizardState = "live"   // no yaml: installer only
	wizFresh  wizardState = "fresh"  // yaml, no k8s: init + join
	wizWorker wizardState = "worker" // k8s without admin.conf: nothing
	wizCP     wizardState = "cp"     // admin.conf: tokens only
)

// Filesystem seams (overridden in tests).
var (
	wizardYAMLPath  = "/run/simplek8s/simplek8s.yaml"
	wizardKubeDir   = "/etc/kubernetes"
	wizardAdminConf = "/etc/kubernetes/admin.conf"
	wizardIssueDir  = "/run/issue.d"
	wizardHostsPath = "/etc/hosts"
	wizardSysBlock  = "/sys/block"
	wizardMountinfo = "/proc/self/mountinfo"
)

//go:embed web
var wizardWeb embed.FS

func detectWizardState() wizardState {
	if _, err := os.Stat(wizardYAMLPath); err != nil {
		return wizLive
	}
	if _, err := os.Stat(wizardKubeDir); err != nil {
		return wizFresh
	}
	if _, err := os.Stat(wizardAdminConf); err != nil {
		return wizWorker
	}
	return wizCP
}

// runWizard implements `nodectl wizard [--listen 0.0.0.0:5443]`.
func runWizard(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("wizard", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var listen string
	fs.StringVar(&listen, "listen", "0.0.0.0:5443", "HTTPS listen address (host:port)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitMisuse
	}
	if fs.NArg() != 0 {
		subcommandUsage(fs, "Usage: nodectl wizard [--listen 0.0.0.0:5443]")
		return exitMisuse
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		log.Error("invalid listen address (want host:port)", "listen", listen)
		return exitMisuse
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		log.Error("invalid listen port", "listen", listen)
		return exitMisuse
	}
	if code := requireRoot(log); code != exitOK {
		return code
	}
	if detectWizardState() == wizWorker {
		log.Info("no management actions available (worker node)")
		return exitIdle
	}
	cert, err := wizardEphemeralCert()
	if err != nil {
		log.Error("generating TLS cert failed", "err", err)
		return exitOperational
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Error("listen failed", "listen", listen, "err", err)
		return exitOperational
	}
	mux := wizardMux(log)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Server errors (TLS handshakes included) go through
		// slog like everything else, not the std logger.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	if err := writeWizardIssue(log, port); err != nil {
		log.Warn("writing issue.d file failed (serving anyway)", "err", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Info("wizard listening", "listen", "https://"+listen)
	serveErr := srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	removeWizardIssue(log)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Error("serve failed", "err", serveErr)
		return exitOperational
	}
	return exitOK
}

// wizardMux wires the SPA plus the JSON API.
func wizardMux(log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", stateHandler(log))
	mux.HandleFunc("POST /api/login", loginHandler(log))
	mux.HandleFunc("POST /api/logout", withSession(log, nil, logoutHandler(log)))
	mux.HandleFunc("GET /api/disks", withSession(log, []wizardState{wizLive}, disksHandler(log)))
	mux.HandleFunc("GET /api/versions", withSession(log, []wizardState{wizLive}, versionsHandler(log)))
	mux.HandleFunc("POST /api/install", withSession(log, []wizardState{wizLive}, installHandler(log)))
	mux.HandleFunc("POST /api/reboot", withSession(log, []wizardState{wizLive}, rebootHandler(log)))
	mux.HandleFunc("GET /api/jobs/{id}", withSession(log, nil, jobHandler(log)))
	mux.HandleFunc("POST /api/kubeadm/init", withSession(log, []wizardState{wizFresh}, kubeInitHandler(log)))
	mux.HandleFunc("POST /api/kubeadm/join", withSession(log, []wizardState{wizFresh}, kubeJoinHandler(log)))
	mux.HandleFunc("GET /api/kubeadm/tokens", withSession(log, []wizardState{wizCP}, kubeTokenListHandler(log)))
	mux.HandleFunc("GET /api/network", withSession(log, []wizardState{wizFresh}, networkHandler(log)))
	mux.HandleFunc("POST /api/kubeadm/tokens", withSession(log, []wizardState{wizCP}, kubeTokenCreateHandler(log)))
	mux.HandleFunc("DELETE /api/kubeadm/tokens/{id}", withSession(log, []wizardState{wizCP}, kubeTokenDeleteHandler(log)))
	mux.HandleFunc("GET /", spaHandler(log))
	return mux
}

// rebootHandler reboots the node after a successful install
// (`live` only): the SPA offers it in the success block so
// the operator proceeds straight to the installed system.
func rebootHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Confirm bool `json:"confirm"`
		}
		if err := readJSON(w, r, &req); err != nil || !req.Confirm {
			apiError(w, http.StatusBadRequest, "explicit confirm required")
			return
		}
		// Start, don't wait: the machine goes down while the
		// 202 response is still being flushed.
		cmd := exec.Command("reboot")
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			log.Error("reboot failed", "err", err)
			apiError(w, http.StatusInternalServerError, "reboot failed")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	}
}

// stateHandler reports the node state for SPA rendering
// (unauthenticated by design — like the listening port
// itself, it reveals no secrets).
func stateHandler(log *slog.Logger) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"state": string(detectWizardState())})
	}
}

// wizardEphemeralCert mints a one-boot EC P-256 self-signed
// cert (M9 D3): no files, no state, no fingerprint notice.
func wizardEphemeralCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "simplek8s-wizard"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"simplek8s", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return tls.Certificate{}, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// wizardIssueFile is the daemon-owned agetty notice, ordered
// between the distro's 10-header and 99-footer (M9 D7). The
// \4 escape is expanded by agetty per login: no IP detection
// in Go, never stale on DHCP change.
func wizardIssueFile() string { return wizardIssueDir + "/50-wizard.issue" }

func wizardIssueText(port int) string {
	return fmt.Sprintf("    Wizard: https://\\4:%d\n", port)
}

func writeWizardIssue(log *slog.Logger, port int) error {
	_ = log
	tmp, err := os.CreateTemp(wizardIssueDir, ".wizard-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.WriteString(tmp, wizardIssueText(port)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, wizardIssueFile())
}

func removeWizardIssue(log *slog.Logger) {
	_ = log
	if err := os.Remove(wizardIssueFile()); err != nil && !os.IsNotExist(err) {
		log.Warn("removing issue.d file failed", "err", err)
	}
}

// spaHandler serves the embedded SPA (no-store: a setup UI
// must never render stale). Unknown paths fall back to
// index.html for client-side routing; /api is never shadowed
// (mux prefers the registered API patterns).
func spaHandler(log *slog.Logger) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if p == "." || p == "" {
			p = "index.html"
		}
		data, err := wizardWeb.ReadFile("web/" + p)
		if err != nil {
			data, err = wizardWeb.ReadFile("web/index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			p = "index.html"
		}
		switch path.Ext(p) {
		case ".html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// writeJSON encodes v with the status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the uniform error shape.
func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON decodes a size-capped strict body.
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
