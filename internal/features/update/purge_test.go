package update

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTargetFreeFromPercent(t *testing.T) {
	cases := []struct {
		total      uint64
		maxPercent int
		want       uint64
	}{
		{1000, 50, 500},
		{1000, 0, 0},   // no cap
		{1000, -5, 0},  // no cap
		{1000, 100, 0}, // no cap
		{1000, 150, 0}, // no cap
		{1001, 99, 10}, // rounding
	}
	for _, c := range cases {
		if got := targetFreeFromPercent(c.total, c.maxPercent); got != c.want {
			t.Errorf("targetFreeFromPercent(%d, %d) = %d, want %d", c.total, c.maxPercent, got, c.want)
		}
	}
}

func TestPlanPurgeNoOpWhenFreeMeetsTarget(t *testing.T) {
	entries := []kernelEntry{
		{ts: "202601010000", name: "k1", size: 100},
		{ts: "202602010000", name: "k2", size: 100},
	}
	if got := planPurge(entries, nil, 0, 1000, 0); got != nil {
		t.Fatalf("planPurge = %v, want nil (free ok)", got)
	}
}

func TestPlanPurgeDeletesOldestFirst(t *testing.T) {
	entries := []kernelEntry{
		{ts: "202601010000", name: "k1", size: 100},
		{ts: "202602010000", name: "k2", size: 100},
		{ts: "202603010000", name: "k3", size: 100},
	}
	got := planPurge(entries, nil, 0, 0, 150)
	if !reflect.DeepEqual(got, []string{"k1", "k2"}) {
		t.Fatalf("planPurge = %v, want [k1 k2]", got)
	}
}

func TestPlanPurgeKeepsNewest(t *testing.T) {
	entries := []kernelEntry{
		{ts: "202601010000", name: "k1", size: 100},
		{ts: "202602010000", name: "k2", size: 100},
		{ts: "202603010000", name: "k3", size: 100},
	}
	// keepNewest=1 protects k3 even though it is not "running".
	got := planPurge(entries, nil, 1, 0, 250)
	if !reflect.DeepEqual(got, []string{"k1", "k2"}) {
		t.Fatalf("planPurge = %v, want [k1 k2] (k3 kept newest)", got)
	}
}

func TestPlanPurgeNeverDeletesProtected(t *testing.T) {
	entries := []kernelEntry{
		{ts: "202601010000", name: "k1", size: 100}, // running
		{ts: "202602010000", name: "k2", size: 100},
		{ts: "202603010000", name: "k3", size: 100},
	}
	protected := map[string]bool{"202601010000": true}
	// Target is unreachable without deleting the protected oldest: the
	// planner must stop and never list k1.
	got := planPurge(entries, protected, 0, 0, 999)
	if !reflect.DeepEqual(got, []string{"k2", "k3"}) {
		t.Fatalf("planPurge = %v, want [k2 k3] (k1 protected)", got)
	}
	for _, n := range got {
		if n == "k1" {
			t.Fatalf("protected kernel k1 was scheduled for deletion")
		}
	}
}

func TestApplyPurgeRemovesOnlyNamedKernels(t *testing.T) {
	root := t.TempDir()
	dir := "simplek8s"
	if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"simplek8s.202601010000.x86-64.efi", "simplek8s.202602010000.x86-64.efi"} {
		if err := os.WriteFile(filepath.Join(root, dir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyPurge(root, dir, []string{"simplek8s.202601010000.x86-64.efi"}); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(root, dir, "simplek8s.202601010000.x86-64.efi")) {
		t.Fatal("purged kernel still present")
	}
	if !fileExists(filepath.Join(root, dir, "simplek8s.202602010000.x86-64.efi")) {
		t.Fatal("untouched kernel was removed")
	}
}
