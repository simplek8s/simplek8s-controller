package main

// Unit tests for the selfupdate pure helpers (PLAN.md §3.14):
// dry-run argv scan, state timestamp round-trip/corruption, and the
// 24h throttle decision. Network/install/exec paths are not unit
// tested (need a node); they are covered by the S-cases in §7.7.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHasDryRunFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"202609171200"}, false},
		{[]string{"--dry-run"}, true},
		{[]string{"-dry-run"}, true},
		{[]string{"--dry-run=true"}, true},
		{[]string{"-dry-run=false"}, true},
		{[]string{"--url", "dev", "--dry-run"}, true},
		{[]string{"--dry-runner"}, false},
		{[]string{"dry-run"}, false},
		{[]string{"--keyring", "/dev/null"}, false},
	} {
		if got := hasDryRunFlag(tc.args); got != tc.want {
			t.Errorf("hasDryRunFlag(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestSelfStateDue(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"zero record", time.Time{}, true},
		{"25h ago", now.Add(-25 * time.Hour), true},
		{"exactly 24h", now.Add(-24 * time.Hour), true},
		{"23h ago", now.Add(-23 * time.Hour), false},
		{"just now", now, false},
		{"future clock", now.Add(time.Hour), false},
	} {
		if got := selfStateDue(tc.last, now); got != tc.want {
			t.Errorf("selfStateDue(%v) = %v, want %v", tc.last, got, tc.want)
		}
	}
}

func TestSelfStateFormatRoundTrip(t *testing.T) {
	// The on-disk format must survive a write/parse cycle at
	// second precision (RFC3339).
	want := time.Date(2026, 9, 21, 12, 34, 56, 0, time.UTC)
	raw := want.Format(time.RFC3339) + "\n"
	got, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip = %v, want %v", got, want)
	}
}

func TestDownloadAndInstall(t *testing.T) {
	payload := []byte("fake-nodectl-bytes-1234")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	ctx := context.Background()

	dir := t.TempDir()
	target := filepath.Join(dir, "nodectl")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := downloadAndInstall(ctx, srv.Client(), srv.URL, "nodectl.ts.arm64", want, target); err != nil {
		t.Fatalf("downloadAndInstall: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed %q, want payload", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, ".nodectl-update-*")); len(entries) != 0 {
		t.Fatalf("leftover temp files: %v", entries)
	}

	// Checksum mismatch: error, target untouched, no leftovers.
	before := append([]byte(nil), got...)
	if err := downloadAndInstall(ctx, srv.Client(), srv.URL, "nodectl.ts.arm64", "00"+want[2:], target); err == nil {
		t.Fatal("want checksum error")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("target modified on checksum failure")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, ".nodectl-update-*")); len(entries) != 0 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestAutoCheckCommand(t *testing.T) {
	for _, cmd := range []string{"check", "update", "list", "purge", "boot", "install", "bogus", "frobnicate", ""} {
		if !autoCheckCommand(cmd) {
			t.Errorf("autoCheckCommand(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range []string{"version", "-version", "--version", "help", "-h", "-help", "--help", "selfupdate"} {
		if autoCheckCommand(cmd) {
			t.Errorf("autoCheckCommand(%q) = true, want false", cmd)
		}
	}
}

func TestDisplayRelease(t *testing.T) {
	sums := map[string]string{
		"nodectl.202602020000.x86-64": "bb",
		"nodectl.202603030000.arm64":  "cc",
	}
	if got := displayRelease(sums, "x86-64", "bb"); got != "202602020000" {
		t.Errorf("indexed sha displays as %q, want ts", got)
	}
	// Same sha under another arch must not match.
	if got := displayRelease(sums, "arm64", "bb"); got == "202602020000" {
		t.Errorf("cross-arch sha displays as %q, want fallback", got)
	}
	// Unknown sha falls back to the full hash.
	full := "a71f7946a841a4fbd843b6f55a8967bf2c032cc678ce14f1a5aa9cdb9aca3e92"
	if got := displayRelease(sums, "x86-64", full); got != full {
		t.Errorf("unknown sha displays as %q, want full sha", got)
	}
}
