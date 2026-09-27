package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Wizard installer (PLAN.md §3.16, `live` only): the M8
// pipeline fronted by a job — validate, resolve, stream,
// partition, write yaml. The typed-echo confirm replaces the
// CLI countdown; config is password mode (default) or
// verbatim advanced mode (no YAML validator for now — init
// validates at boot, M9 D8).

type installRequest struct {
	Device          string `json:"device"`
	Channel         string `json:"channel"`
	TS              string `json:"ts"`
	Mode            string `json:"mode"`
	Password        string `json:"password"`
	PasswordConfirm string `json:"passwordConfirm"`
	SSHPublicKeys   string `json:"sshPublicKeys"`
	ConfigYAML      string `json:"configYaml"`
	ConfirmDevice   string `json:"confirmDevice"`
}

// sshKeyLine matches one authorized_keys line (type + base64,
// optional comment). Keys travel one-per-line in a textarea.
var sshKeyLine = regexp.MustCompile(`^(ssh-(rsa|dss|ed25519)|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]+( .*)?$`)

// parseSSHPublicKeys splits the textarea, skips blank lines and
// rejects the first malformed one with its line number.
func parseSSHPublicKeys(text string) ([]string, error) {
	var keys []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !sshKeyLine.MatchString(line) {
			return nil, fmt.Errorf("ssh key line %d is not a valid public key", i+1)
		}
		keys = append(keys, line)
	}
	return keys, nil
}

// parseInstallRequest validates the wire shape (pure: unit
// tested). Heavyweight checks (device, index) run after.
func parseInstallRequest(req installRequest) (mode string, passwordHash string, keys []string, configYAML string, err error) {
	if strings.TrimSpace(req.Device) == "" {
		return "", "", nil, "", fmt.Errorf("device is required")
	}
	if req.ConfirmDevice != req.Device {
		return "", "", nil, "", fmt.Errorf("confirm device does not match (type %q to confirm)", req.Device)
	}
	mode = req.Mode
	if mode == "" {
		mode = "password"
	}
	switch mode {
	case "password":
		if req.Password == "" {
			return "", "", nil, "", fmt.Errorf("root password is required")
		}
		if req.Password != req.PasswordConfirm {
			return "", "", nil, "", fmt.Errorf("passwords do not match")
		}
		hash, herr := hashRootPassword(req.Password)
		if herr != nil {
			return "", "", nil, "", fmt.Errorf("hashing password failed")
		}
		keys, kerr := parseSSHPublicKeys(req.SSHPublicKeys)
		if kerr != nil {
			return "", "", nil, "", kerr
		}
		return mode, hash, keys, "", nil
	case "config":
		if strings.TrimSpace(req.ConfigYAML) == "" {
			return "", "", nil, "", fmt.Errorf("config yaml is required")
		}
		return mode, "", nil, req.ConfigYAML, nil
	default:
		return "", "", nil, "", fmt.Errorf("unknown mode %q (want password|config)", req.Mode)
	}
}

func installHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req installRequest
		if err := readJSON(w, r, &req); err != nil {
			apiError(w, http.StatusBadRequest, "invalid request")
			return
		}
		channel := req.Channel
		if channel == "" {
			channel = "stable"
		}
		mode, passwordHash, keys, configYAML, err := parseInstallRequest(req)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		target, base, code := validateInstallTarget(log, req.Device)
		if code != exitOK {
			apiError(w, exitCodeStatus(code), "target refused (see node log)")
			return
		}
		if sz, err := installTargetSize(base); err != nil {
			log.Error("reading target size failed", "err", err)
			apiError(w, http.StatusInternalServerError, "cannot size target")
			return
		} else if sz < installSizeFloor {
			apiError(w, http.StatusBadRequest, "target too small (need at least 1GiB)")
			return
		}
		baseURL := repoBase(channel)
		sums, err := cachedIndex(r.Context(), log, baseURL)
		if err != nil {
			log.Error("install index failed", "err", err)
			apiError(w, http.StatusBadGateway, "release channel unreachable")
			return
		}
		flavor, ok := wizardFlavor()
		if !ok {
			apiError(w, http.StatusInternalServerError, "install flavor unresolvable")
			return
		}
		ts, file, sum, selErr := selectInstallRelease(sums, flavor, normalizeTS(req.TS))
		if selErr != nil {
			// A pinned ts the operator chose is their error
			// (400); no usable latest is ours (502).
			if normalizeTS(req.TS) != "" {
				apiError(w, http.StatusBadRequest, selErr.Error())
			} else {
				apiError(w, http.StatusBadGateway, selErr.Error())
			}
			return
		}
		c, _ := r.Cookie(wizardSessionCookie)
		owner := ""
		if c != nil {
			owner = c.Value
		}
		j, started := jobs.start("install", owner, true)
		if !started {
			apiError(w, http.StatusConflict, "another operation is already running")
			return
		}
		j.setTotal(installCompressedSize(baseURL, file))
		go runInstallJob(log, j, target, baseURL, file, sum, ts, mode, passwordHash, keys, configYAML)
		writeJSON(w, http.StatusAccepted, map[string]string{"job": j.id})
	}
}

// normalizeTS maps the UI's "latest" (and blanks) to the
// empty selector meaning newest.
func normalizeTS(ts string) string {
	if strings.TrimSpace(ts) == "" || ts == "latest" {
		return ""
	}
	return ts
}

// installCompressedSize HEADs the artifact for the progress
// percent denominator (best-effort: 0 when unknown).
func installCompressedSize(baseURL, file string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, baseURL+"/"+file, nil)
	if err != nil {
		return 0
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.ContentLength <= 0 {
		return 0
	}
	return resp.ContentLength
}

// exitCodeStatus maps CLI exits to HTTP for pre-job refusals.
func exitCodeStatus(code int) int {
	if code == exitMisuse {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// runInstallJob executes the M8 pipeline inside a job.
func runInstallJob(log *slog.Logger, j *wizardJob, target, baseURL, file, sum, ts, mode, passwordHash string, keys []string, configYAML string) {
	defer jobs.release()
	ctx := context.Background()
	j.say("streaming " + file + " onto " + target)
	if err := streamImageToDiskReport(ctx, httpClient(), baseURL, file, sum, target, j.report); err != nil {
		log.Error("wizard install stream failed", "err", err)
		j.finish(jobFailed, "", "install failed (target left dirty — retry)")
		return
	}
	if code := appendVarPartition(log, target); code != exitOK {
		j.finish(jobFailed, "", "partitioning /var failed")
		return
	}
	cfgPath := ""
	if mode == "config" {
		tmp, err := os.CreateTemp("", "wizard-config-*.yaml")
		if err != nil {
			log.Error("wizard config scratch failed", "err", err)
			j.finish(jobFailed, "", "writing config failed")
			return
		}
		cfgPath = tmp.Name()
		defer os.Remove(cfgPath)
		if _, err := tmp.WriteString(configYAML); err != nil {
			tmp.Close()
			j.finish(jobFailed, "", "writing config failed")
			return
		}
		tmp.Close()
	}
	if code := writeInstallYAML(log, partDevName(target, 1), cfgPath, ts, passwordHash, keys); code != exitOK {
		j.finish(jobFailed, "", "writing simplek8s.yaml failed")
		return
	}
	j.finish(jobDone, installSummaryLine(target, ts), "")
}
