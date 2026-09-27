package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Wizard kubeadm management (PLAN.md §3.16): init, join and
// tokens as thin `argv` wrappers over the distro binary — no
// shell, secrets masked in job logs. States gate each route
// (fresh: init/join, cp: tokens).

var (
	// wizardKubeadmPath is the distro wrapper (M9 D9).
	wizardKubeadmPath = "/usr/local/bin/kubeadm"
	wizardKubeTimeout = 30 * time.Minute
)

const wizardPodCIDR = "10.244.0.0/16"

var (
	reTokenID     = regexp.MustCompile(`^[a-z0-9]{6}$`)
	reTokenSecret = regexp.MustCompile(`^[a-z0-9]{16}$`)
	reTokenFull   = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	reCAHash      = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	reCertKey     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	reHostname    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reTokenDelete = regexp.MustCompile(`^[a-z0-9]{6}(\.[a-z0-9]{16})?$`)
)

// joinConfig is the single-blob join input (M9 D10).
type joinConfig struct {
	Address        string `json:"address"`
	Token          string `json:"token"`
	CACertHash     string `json:"caCertHash"`
	CertificateKey string `json:"certificateKey"`
	Role           string `json:"role"`
	Hostname       string `json:"hostname"`
}

// parseJoinConfig accepts the blob as an object or as a JSON
// string (one textarea either way) and validates every field.
func parseJoinConfig(raw json.RawMessage) (joinConfig, error) {
	var jc joinConfig
	// String-wrapped form: {"config": "<json>"} arrives here as
	// the inner string; try it first, fall back to object.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		raw = json.RawMessage(s)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&jc); err != nil {
		return joinConfig{}, fmt.Errorf("invalid join JSON: %v", err)
	}
	if err := jc.validate(); err != nil {
		return joinConfig{}, err
	}
	return jc, nil
}

func (jc joinConfig) validate() error {
	host, port, err := net.SplitHostPort(jc.Address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("address must be host:port (port required)")
	}
	if p, err := net.LookupPort("tcp", port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("address has an invalid port")
	}
	if !reTokenFull.MatchString(jc.Token) {
		return fmt.Errorf("token must look like xxxxxx.xxxxxxxxxxxxxxxx")
	}
	if !reCAHash.MatchString(jc.CACertHash) {
		return fmt.Errorf("caCertHash must look like sha256:<64 hex>")
	}
	switch jc.Role {
	case "worker":
		if jc.CertificateKey != "" {
			return fmt.Errorf("certificateKey is forbidden for worker (wrong blob?)")
		}
	case "control-plane":
		if !reCertKey.MatchString(jc.CertificateKey) {
			return fmt.Errorf("certificateKey is required for control-plane (64 hex)")
		}
	default:
		return fmt.Errorf("role must be worker|control-plane")
	}
	if !reHostname.MatchString(jc.Hostname) {
		return fmt.Errorf("hostname must be lowercase alphanumeric/hyphens (RFC 1123)")
	}
	return nil
}

// buildJoinArgs renders the validated config as kubeadm argv.
func buildJoinArgs(jc joinConfig) []string {
	args := []string{"join", jc.Address,
		"--token", jc.Token,
		"--discovery-token-ca-cert-hash", jc.CACertHash,
	}
	if jc.Role == "control-plane" {
		args = append(args, "--control-plane", "--certificate-key", jc.CertificateKey)
	}
	return append(args, "--node-name", jc.Hostname)
}

// editHostsContent applies the 127.0.1.1 rule idempotently:
// replace the line when present, append it when absent.
// 127.0.0.1 and everything else pass through byte-identical.
func editHostsContent(content, hostname string) string {
	want := "127.0.1.1 " + hostname
	if content == "" {
		return want + "\n"
	}
	trailingNL := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	replaced := false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if f := strings.Fields(trimmed); len(f) > 0 && f[0] == "127.0.1.1" {
			lines[i] = want
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, want)
	}
	out := strings.Join(lines, "\n")
	if trailingNL {
		out += "\n"
	}
	return out
}

// applyHostname sets the node name before joining:
// hostnamectl plus the /etc/hosts rule. A failed join keeps
// the new hostname (diagnosable, documented).
func applyHostname(log *slog.Logger, hostname string) error {
	if out, err := exec.Command("hostnamectl", "set-hostname", hostname).CombinedOutput(); err != nil {
		log.Error("set-hostname failed", "err", err, "out", strings.TrimSpace(string(out)))
		return fmt.Errorf("set-hostname failed")
	}
	raw, err := os.ReadFile(wizardHostsPath)
	if err != nil {
		log.Error("reading hosts failed", "err", err)
		return fmt.Errorf("reading hosts failed")
	}
	if err := os.WriteFile(wizardHostsPath, []byte(editHostsContent(string(raw), hostname)), 0o644); err != nil {
		log.Error("writing hosts failed", "err", err)
		return fmt.Errorf("writing hosts failed")
	}
	return nil
}

// ensureRuntimes prepares containerd and kubelet before
// init/join (idempotent): a fresh install may carry them
// disabled despite an enabling preset, and init must not
// depend on distro boot state. containerd is started and its
// CRI socket awaited. kubelet is only ENABLED, never started
// here: the distro drop-in gates its start on
// /var/lib/kubelet/config.yaml, which exists only after
// kubeadm finishes — starting it now deadlocks init (the
// start-pre waiter never returns). kubeadm itself starts
// kubelet once it writes the config.
func ensureRuntimes(ctx context.Context, log *slog.Logger, j *wizardJob) error {
	for _, unit := range []string{"containerd", "kubelet"} {
		cmd := exec.CommandContext(ctx, "systemctl", "enable", unit)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			log.Error("enabling unit failed", "unit", unit, "err", err, "out", maskSecrets(strings.TrimSpace(buf.String()), nil))
			return fmt.Errorf("enabling %s failed", unit)
		}
	}
	{
		cmd := exec.CommandContext(ctx, "systemctl", "start", "containerd")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			log.Error("starting containerd failed", "err", err, "out", maskSecrets(strings.TrimSpace(buf.String()), nil))
			return fmt.Errorf("starting containerd failed")
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat("/var/run/containerd/containerd.sock"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("containerd socket did not appear")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// maskSecrets replaces each secret with *** (job logs, errors).
func maskSecrets(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// buildInitArgs renders the init argv: the operator supplies
// the advertise address and the node hostname; the pod CIDR
// is fixed and the control-plane endpoint defaults to the
// same address (stable endpoint required before any second
// control plane joins; point it at a load balancer later
// with kubeadm).
func buildInitArgs(advertiseAddr, hostname string) []string {
	return []string{"init",
		"--apiserver-advertise-address=" + advertiseAddr,
		"--control-plane-endpoint=" + advertiseAddr + ":6443",
		"--pod-network-cidr=" + wizardPodCIDR,
		"--node-name=" + hostname,
	}
}

// streaming masked lines into the job log. It returns the raw
// output for callers that must parse secrets out of it.
// kubeadmRun execs the distro binary (argv, never a shell),
// kubeadmRun execs the distro binary (argv, never a shell),
// streaming masked lines into the job log as they arrive. It
// returns the raw output for callers that must parse secrets
// out of it.
func kubeadmRun(ctx context.Context, log *slog.Logger, j *wizardJob, args, secrets []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wizardKubeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, wizardKubeadmPath, args...)
	var buf bytes.Buffer
	pr, pw := io.Pipe()
	cmd.Stdout = io.MultiWriter(pw, &buf)
	cmd.Stderr = io.MultiWriter(pw, &buf)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := maskSecrets(strings.TrimSpace(sc.Text()), secrets); line != "" && j != nil {
				j.say(line)
			}
		}
	}()
	err := cmd.Run()
	_ = pw.Close()
	wg.Wait()
	if err != nil {
		log.Error("kubeadm failed", "args", maskSecrets(strings.Join(args, " "), secrets), "err", err)
		return buf.String(), fmt.Errorf("kubeadm %s failed", args[0])
	}
	return buf.String(), nil
}

// manualSetupCommands renders the two pre-join shell commands
// for the command-line alternative: set the hostname, then
// idempotently (re)place the 127.0.1.1 hosts line.
func manualSetupCommands(hostname string) (hostCmd, hostsCmd string) {
	hostCmd = "hostnamectl set-hostname " + hostname
	hostsCmd = "sed -i '/^127\\.0\\.1\\.1 /d' /etc/hosts && echo '127.0.1.1 " + hostname + "' >> /etc/hosts"
	return hostCmd, hostsCmd
}

// parseJoinCommand extracts address/token/ca-hash from a
// `kubeadm token create --print-join-command` line.
func parseJoinCommand(out string) (addr, token, caHash string, err error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 3 || f[0] != "kubeadm" || f[1] != "join" {
			continue
		}
		addr = f[2]
		for i := 3; i < len(f)-1; i++ {
			switch f[i] {
			case "--token":
				token = f[i+1]
			case "--discovery-token-ca-cert-hash":
				caHash = f[i+1]
			}
		}
		if addr == "" || token == "" || caHash == "" {
			continue
		}
		jc := joinConfig{Address: addr, Token: token, CACertHash: caHash, Role: "worker", Hostname: "x"}
		if verr := jc.validate(); verr != nil {
			continue
		}
		return addr, token, caHash, nil
	}
	return "", "", "", fmt.Errorf("no parseable join command in output")
}

// bootstrapTokenOut is one `token list -o json` document.
// kubeadm prints a single object for one token and a bare
// stream of objects for several (never a List and never one
// JSON value): decode accordingly.
type bootstrapTokenOut struct {
	Kind        string `json:"kind"`
	Token       string `json:"token"`
	Description string `json:"description"`
	Expires     string `json:"expires"`
}

// decodeTokenStream parses the object stream.
func decodeTokenStream(data []byte) []bootstrapTokenOut {
	var out []bootstrapTokenOut
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var t bootstrapTokenOut
		if err := dec.Decode(&t); err != nil {
			break
		}
		if t.Token != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseTokenExpiry finds a bootstrap token's expiration
// (best-effort: "" when absent).
func parseTokenExpiry(listJSON []byte, tokenID string) string {
	for _, t := range decodeTokenStream(listJSON) {
		parts := strings.SplitN(t.Token, ".", 2)
		if len(parts) == 2 && parts[0] == tokenID {
			return t.Expires
		}
	}
	return ""
}

func kubeInitHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AdvertiseAddress string `json:"advertiseAddress"`
			Hostname         string `json:"hostname"`
		}
		if err := readJSON(w, r, &req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if net.ParseIP(strings.TrimSpace(req.AdvertiseAddress)) == nil {
			apiError(w, http.StatusBadRequest, "advertiseAddress must be an IP")
			return
		}
		if !reHostname.MatchString(req.Hostname) {
			apiError(w, http.StatusBadRequest, "hostname must be lowercase alphanumeric/hyphens (RFC 1123)")
			return
		}
		c, _ := r.Cookie(wizardSessionCookie)
		owner := ""
		if c != nil {
			owner = c.Value
		}
		j, started := jobs.start("init", owner, true)
		if !started {
			apiError(w, http.StatusConflict, "another operation is already running")
			return
		}
		addr := strings.TrimSpace(req.AdvertiseAddress)
		hostname := req.Hostname
		go func() {
			defer jobs.release()
			args := buildInitArgs(addr, hostname)
			j.say("setting hostname to " + hostname)
			if err := applyHostname(log, hostname); err != nil {
				j.finish(jobFailed, "", "setting hostname failed")
				return
			}
			if err := ensureRuntimes(context.Background(), log, j); err != nil {
				j.finish(jobFailed, "", err.Error())
				return
			}
			if _, err := kubeadmRun(context.Background(), log, j, args, []string{addr}); err != nil {
				j.finish(jobFailed, "", "kubeadm init failed")
				return
			}
			j.finish(jobDone, "cluster initialized (this node is a control plane)", "")
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"job": j.id})
	}
}

func kubeJoinHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Config json.RawMessage `json:"config"`
		}
		if err := readJSON(w, r, &req); err != nil || len(req.Config) == 0 {
			apiError(w, http.StatusBadRequest, "config with the join JSON is required")
			return
		}
		jc, err := parseJoinConfig(req.Config)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		c, _ := r.Cookie(wizardSessionCookie)
		owner := ""
		if c != nil {
			owner = c.Value
		}
		j, started := jobs.start("join", owner, true)
		if !started {
			apiError(w, http.StatusConflict, "another operation is already running")
			return
		}
		secrets := []string{jc.Token, jc.CACertHash, jc.CertificateKey}
		go func() {
			defer jobs.release()
			j.say("setting hostname to " + jc.Hostname)
			if err := applyHostname(log, jc.Hostname); err != nil {
				j.finish(jobFailed, "", "setting hostname failed")
				return
			}
			if err := ensureRuntimes(context.Background(), log, j); err != nil {
				j.finish(jobFailed, "", err.Error())
				return
			}
			if _, err := kubeadmRun(context.Background(), log, j, buildJoinArgs(jc), secrets); err != nil {
				j.finish(jobFailed, "", "kubeadm join failed (hostname kept: "+jc.Hostname+")")
				return
			}
			j.finish(jobDone, "joined as "+jc.Role+" ("+jc.Hostname+")", "")
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"job": j.id})
	}
}

type tokenCreateRequest struct {
	Role     string `json:"role"`
	Hostname string `json:"hostname"`
}

// kubeTokenCreateHandler mints one token for one joining node
// (one-token-per-node flow, M9 D11): fresh certificate key for
// control-plane joins (generated by us — nothing parsed),
// upload-certs, then the token with its join command. Tokens
// stay reusable until TTL/delete (upstream property);
// expiry is reported and revoke is offered.
func kubeTokenCreateHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req tokenCreateRequest
		if err := readJSON(w, r, &req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid request")
			return
		}
		if req.Role != "worker" && req.Role != "control-plane" {
			apiError(w, http.StatusBadRequest, "role must be worker|control-plane")
			return
		}
		if !reHostname.MatchString(req.Hostname) {
			apiError(w, http.StatusBadRequest, "hostname must be lowercase alphanumeric/hyphens (RFC 1123)")
			return
		}
		ctx := r.Context()
		secrets := []string{}
		var certKey string
		if req.Role == "control-plane" {
			out, err := kubeadmRun(ctx, log, nil, []string{"certs", "certificate-key"}, nil)
			if err != nil {
				apiError(w, http.StatusInternalServerError, "generating certificate key failed")
				return
			}
			certKey = strings.TrimSpace(out)
			if !reCertKey.MatchString(certKey) {
				log.Error("certificate key has an unexpected shape")
				apiError(w, http.StatusInternalServerError, "generating certificate key failed")
				return
			}
			secrets = append(secrets, certKey)
			if _, err := kubeadmRun(ctx, log, nil, []string{"init", "phase", "upload-certs", "--upload-certs", "--certificate-key", certKey}, secrets); err != nil {
				apiError(w, http.StatusInternalServerError, "uploading certs failed")
				return
			}
		}
		args := []string{"token", "create", "--description", "wizard:" + req.Hostname, "--print-join-command"}
		if certKey != "" {
			args = append(args, "--certificate-key", certKey)
		}
		out, err := kubeadmRun(ctx, log, nil, args, secrets)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "creating token failed")
			return
		}
		addr, token, caHash, err := parseJoinCommand(out)
		if err != nil {
			log.Error("join command parse failed", "err", err)
			apiError(w, http.StatusInternalServerError, "creating token failed")
			return
		}
		secrets = append(secrets, token, caHash)
		_ = secrets
		workerCmd := fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s --node-name %s", addr, token, caHash, req.Hostname)
		workerJSON, _ := json.Marshal(joinConfig{Address: addr, Token: token, CACertHash: caHash, Role: "worker", Hostname: req.Hostname})
		hostCmd, hostsCmd := manualSetupCommands(req.Hostname)
		resp := map[string]any{
			"role":            req.Role,
			"hostname":        req.Hostname,
			"expires":         tokenExpiry(ctx, log, token),
			"hostnameCommand": hostCmd,
			"hostsCommand":    hostsCmd,
		}
		if req.Role == "worker" {
			resp["command"] = hostCmd + "\n\n" + hostsCmd + "\n\n" + workerCmd
			resp["joinJson"] = string(workerJSON)
		} else {
			cpCmd := workerCmd + " --control-plane --certificate-key " + certKey
			resp["command"] = hostCmd + "\n\n" + hostsCmd + "\n\n" + cpCmd
			cpJSON, _ := json.Marshal(joinConfig{Address: addr, Token: token, CACertHash: caHash, CertificateKey: certKey, Role: "control-plane", Hostname: req.Hostname})
			resp["joinJson"] = string(cpJSON)
			resp["workerCommand"] = workerCmd
			resp["workerJoinJson"] = string(workerJSON)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// tokenExpiry reports the token's expiration via the list
// (best-effort: "" when the shape surprises us).
func tokenExpiry(ctx context.Context, log *slog.Logger, token string) string {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || !reTokenID.MatchString(parts[0]) {
		return ""
	}
	out, err := kubeadmRun(ctx, log, nil, []string{"token", "list", "-o", "json"}, []string{token})
	if err != nil {
		return ""
	}
	return parseTokenExpiry([]byte(out), parts[0])
}

func kubeTokenListHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := kubeadmRun(r.Context(), log, nil, []string{"token", "list", "-o", "json"}, nil)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "listing tokens failed")
			return
		}
		type tokenInfo struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			Expires     string `json:"expires"`
		}
		infos := []tokenInfo{}
		for _, t := range decodeTokenStream([]byte(out)) {
			id := strings.SplitN(t.Token, ".", 2)[0]
			infos = append(infos, tokenInfo{ID: id, Description: t.Description, Expires: t.Expires})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": infos})
	}
}

func kubeTokenDeleteHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !reTokenDelete.MatchString(id) {
			apiError(w, http.StatusBadRequest, "invalid token id")
			return
		}
		if _, err := kubeadmRun(r.Context(), log, nil, []string{"token", "delete", id}, []string{id}); err != nil {
			apiError(w, http.StatusInternalServerError, "deleting token failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
