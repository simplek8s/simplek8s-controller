package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.EngineInterval != 2*time.Second {
		t.Errorf("EngineInterval = %v, want 2s", d.EngineInterval)
	}
	if d.MaxConcurrentReboots != 1 {
		t.Errorf("MaxConcurrentReboots = %v, want 1", d.MaxConcurrentReboots)
	}
	if d.OnRebootFailure != "pause" {
		t.Errorf("OnRebootFailure = %q, want pause", d.OnRebootFailure)
	}
	if d.RebootDrainTimeout != 10*time.Minute {
		t.Errorf("RebootDrainTimeout = %v, want 10m", d.RebootDrainTimeout)
	}
	if d.RebootIssueGrace != 5*time.Minute {
		t.Errorf("RebootIssueGrace = %v, want 5m", d.RebootIssueGrace)
	}
	if d.UpdateMode != "off" {
		t.Errorf("UpdateMode = %q, want off", d.UpdateMode)
	}
	if d.UpdateURL != "https://dl.simplek8s.org/simplek8s/stable" {
		t.Errorf("UpdateURL = %q", d.UpdateURL)
	}
	if d.UpdateCheckInterval != 12*time.Hour {
		t.Errorf("UpdateCheckInterval = %v, want 12h", d.UpdateCheckInterval)
	}
	if d.UpdatePreserve != 3 {
		t.Errorf("UpdatePreserve = %v, want 3", d.UpdatePreserve)
	}
	if d.UpdateMaxPercentUsage != 75 {
		t.Errorf("UpdateMaxPercentUsage = %v, want 75", d.UpdateMaxPercentUsage)
	}
}

func TestParseAbsentConfigMapIsDefaults(t *testing.T) {
	got, warns := Parse(Defaults(), nil)
	if warns != nil {
		t.Fatalf("warns = %v, want none", warns)
	}
	if got != Defaults() {
		t.Errorf("absent ConfigMap: got %+v, want defaults", got)
	}
	got, warns = Parse(Defaults(), map[string]string{})
	if warns != nil || got != (Defaults()) {
		t.Errorf("empty data: got %+v warns %v", got, warns)
	}
}

func TestParseAllValidKeys(t *testing.T) {
	data := map[string]string{
		"engine.engine-interval":         "30s",
		"reboots.max-concurrent-reboots": "4",
		"reboots.on-reboot-failure":      "continue",
		"reboots.reboot-drain-timeout":   "1h30m",
		"reboots.reboot-issue-grace":     "90s",
		"updates.update-mode":            "full",
		"updates.url":                    "https://dl.example.org/simplek8s/dev",
		"updates.check-interval":         "6h",
		"updates.preserve":               "5",
		"updates.max-percent-usage":      "90",
	}
	got, warns := Parse(Defaults(), data)
	if warns != nil {
		t.Fatalf("warns = %v, want none", warns)
	}
	want := Config{
		EngineInterval:        30 * time.Second,
		MaxConcurrentReboots:  4,
		OnRebootFailure:       "continue",
		RebootDrainTimeout:    90 * time.Minute,
		RebootIssueGrace:      90 * time.Second,
		UpdateMode:            "full",
		UpdateURL:             "https://dl.example.org/simplek8s/dev",
		UpdateCheckInterval:   6 * time.Hour,
		UpdatePreserve:        5,
		UpdateMaxPercentUsage: 90,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseAbsentKeysKeepDefaults(t *testing.T) {
	data := map[string]string{
		"reboots.max-concurrent-reboots": "2",
	}
	got, warns := Parse(Defaults(), data)
	if warns != nil {
		t.Fatalf("warns = %v, want none", warns)
	}
	want := Defaults()
	want.MaxConcurrentReboots = 2
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseInvalidKeepsLastValidAndWarns(t *testing.T) {
	base := Defaults()
	base.MaxConcurrentReboots = 2
	base.RebootDrainTimeout = time.Hour
	base.UpdateMode = "stage"
	data := map[string]string{
		"reboots.max-concurrent-reboots": "not-a-number",
		"reboots.reboot-drain-timeout":   "-5m",
		"reboots.on-reboot-failure":      "sometimes",
		"reboots.reboot-issue-grace":     "0s",
		"updates.update-mode":            "half",
		"updates.url":                    "ftp://nope.example",
		"updates.check-interval":         "never",
		"updates.preserve":               "0",
		"updates.max-percent-usage":      "101",
		"engine.engine-interval":         "",
	}
	got, warns := Parse(base, data)
	if len(warns) != 10 {
		t.Fatalf("warns = %v, want 10", warns)
	}
	if got != base {
		t.Errorf("got %+v, want base %+v", got, base)
	}
}

func TestParseUnknownKeyWarnsAndIgnores(t *testing.T) {
	got, warns := Parse(Defaults(), map[string]string{
		"reboots.max-concurrent-reboots": "1",
		"future.some-key":                "x",
	})
	if len(warns) != 1 || warns[0] != `unknown config key "future.some-key" ignored` {
		t.Fatalf("warns = %v", warns)
	}
	if got.MaxConcurrentReboots != 1 {
		t.Errorf("known key must still apply, got %+v", got)
	}
}

func TestParseBoundaries(t *testing.T) {
	if _, ok := parseRangeInt("1", 1, 100); !ok {
		t.Error("range min must be accepted")
	}
	if _, ok := parseRangeInt("100", 1, 100); !ok {
		t.Error("range max must be accepted")
	}
	if _, ok := parseMinInt("0", 0); !ok {
		t.Error("min-int 0 must be accepted")
	}
	if _, ok := parseDuration("1ms"); !ok {
		t.Error("tiny positive duration must be accepted")
	}
	if _, ok := parseURL("http://127.0.0.1:8080/repo"); !ok {
		t.Error("http URL with port must be accepted")
	}
	if _, ok := parseURL("https://"); ok {
		t.Error("URL without host must be rejected")
	}
	if _, ok := parseEnum("PAUSE", "pause"); ok {
		t.Error("enum must be case-sensitive")
	}
}
