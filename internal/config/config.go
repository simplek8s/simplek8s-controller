// Package config resolves the controller's feature configuration from
// the flat-key ConfigMap (PLAN-M2 3.2): feature-prefixed keys,
// built-in defaults, last-valid-wins on invalid values, unknown keys
// warned and ignored.
package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Config is one resolved snapshot of all feature settings.
type Config struct {
	// engine
	EngineInterval time.Duration
	// reboots
	MaxConcurrentReboots int
	OnRebootFailure      string // "pause" | "continue"
	RebootDrainTimeout   time.Duration
	RebootIssueGrace     time.Duration
	// updates
	UpdateMode            string // "off" | "stage" | "full"
	UpdateURL             string
	UpdateCheckInterval   time.Duration
	UpdatePreserve        int
	UpdateMaxPercentUsage int
}

// Defaults is the built-in table (PLAN-M2 3.2).
func Defaults() Config {
	return Config{
		EngineInterval:        2 * time.Second,
		MaxConcurrentReboots:  1,
		OnRebootFailure:       "pause",
		RebootDrainTimeout:    10 * time.Minute,
		RebootIssueGrace:      5 * time.Minute,
		UpdateMode:            "off",
		UpdateURL:             "https://dl.simplek8s.org/simplek8s/stable",
		UpdateCheckInterval:   12 * time.Hour,
		UpdatePreserve:        3,
		UpdateMaxPercentUsage: 75,
	}
}

// Parse applies the flat data (ConfigMap .data; nil or empty means the
// ConfigMap is absent) over base (the last valid snapshot or
// Defaults), returning the new snapshot and a warning per unknown or
// invalid key. Invalid values keep base's value; the controller never
// fails on a config typo (PLAN-M2 3.2).
func Parse(base Config, data map[string]string) (Config, []string) {
	out := base
	var warns []string
	if len(data) > 0 {
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			raw := data[k]
			ok := true
			switch k {
			case "engine.engine-interval":
				if v, valid := parseDuration(raw); valid {
					out.EngineInterval = v
				} else {
					ok = false
				}
			case "reboots.max-concurrent-reboots":
				if v, valid := parseMinInt(raw, 0); valid {
					out.MaxConcurrentReboots = v
				} else {
					ok = false
				}
			case "reboots.on-reboot-failure":
				if v, valid := parseEnum(raw, "pause", "continue"); valid {
					out.OnRebootFailure = v
				} else {
					ok = false
				}
			case "reboots.reboot-drain-timeout":
				if v, valid := parseDuration(raw); valid {
					out.RebootDrainTimeout = v
				} else {
					ok = false
				}
			case "reboots.reboot-issue-grace":
				if v, valid := parseDuration(raw); valid {
					out.RebootIssueGrace = v
				} else {
					ok = false
				}
			case "updates.update-mode":
				if v, valid := parseEnum(raw, "off", "stage", "full"); valid {
					out.UpdateMode = v
				} else {
					ok = false
				}
			case "updates.url":
				if v, valid := parseURL(raw); valid {
					out.UpdateURL = v
				} else {
					ok = false
				}
			case "updates.check-interval":
				if v, valid := parseDuration(raw); valid {
					out.UpdateCheckInterval = v
				} else {
					ok = false
				}
			case "updates.preserve":
				if v, valid := parseMinInt(raw, 1); valid {
					out.UpdatePreserve = v
				} else {
					ok = false
				}
			case "updates.max-percent-usage":
				if v, valid := parseRangeInt(raw, 1, 100); valid {
					out.UpdateMaxPercentUsage = v
				} else {
					ok = false
				}
			default:
				warns = append(warns, fmt.Sprintf("unknown config key %q ignored", k))
				continue
			}
			if !ok {
				warns = append(warns, fmt.Sprintf("invalid config value %s=%q; keeping previous", k, raw))
			}
		}
	}
	return out, warns
}

func parseDuration(raw string) (time.Duration, bool) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func parseMinInt(raw string, min int) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		return 0, false
	}
	return n, true
}

func parseRangeInt(raw string, min, max int) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

func parseEnum(raw string, allowed ...string) (string, bool) {
	for _, a := range allowed {
		if raw == a {
			return a, true
		}
	}
	return "", false
}

func parseURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := u.Scheme
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	return raw, true
}
