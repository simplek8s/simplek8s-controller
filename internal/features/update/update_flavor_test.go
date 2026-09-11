package update

import (
	"testing"
)

// Legacy arm64 lineage: a node carrying *.arm64.efi files resolves
// flavor arm64 and updates within its (old) artifacts (PLAN-M5 §3.6).
func TestLegacyArm64LineageUpdates(t *testing.T) {
	h := newUpdateHarness(t, "stage", "6.18.48-simplek8s-202501010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202501010000"},
		[]string{"202501010000"})
	h.store.setFlavor("202501010000", "arm64")
	h.repo.set(t, map[string][]byte{
		"simplek8s.202502020000.arm64.efi.zst": []byte("arm-kernel"),
	})
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 1 || got[0] != "202502020000" {
		t.Fatalf("staged = %v, want the arm64 lineage release", got)
	}
	if got := h.nextKernel(); got != "202502020000" {
		t.Fatalf("next-kernel = %q, want anchored", got)
	}
}

// Ended lineage: newer ts exist only for other flavors → the legacy
// node sees no update (no silent cross-flavor moves).
func TestLegacySeesNoUpdateBeyondLineage(t *testing.T) {
	h := newUpdateHarness(t, "stage", "6.18.48-simplek8s-202501010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202501010000"},
		[]string{"202501010000"})
	h.store.setFlavor("202501010000", "arm64")
	// kernelIndex() has newer x86-64 + aarch64 releases, but nothing arm64.
	h.tick()
	if got := h.store.stagedVersions(); len(got) != 0 {
		t.Fatalf("staged = %v, want nothing (lineage ended)", got)
	}
	if got := h.nextKernel(); got != "202501010000" {
		t.Fatalf("next-kernel = %q, want unchanged", got)
	}
}

// Out-of-flavor pin: the ts exists in the index, but not for this
// node's flavor. With no migration by design (D3), it follows normal
// W12 rules — correct to safe state, like a never-existed ts.
func TestOutOfFlavorPinCorrects(t *testing.T) {
	h := newUpdateHarness(t, "full", "6.18.48-simplek8s-202501010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202608291203"},
		[]string{"202501010000"})
	h.store.setFlavor("202501010000", "arm64")
	// 202608291203 exists as x86-64 (and aarch64) in kernelIndex(),
	// never as arm64.
	h.tick()
	if got := h.nextKernel(); got != "202501010000" {
		t.Fatalf("next-kernel = %q, want corrected to running (not verifiable in flavor)", got)
	}
	if h.eventCount("UpdateGoalCorrected") != 1 {
		t.Fatalf("UpdateGoalCorrected = %d, want 1", h.eventCount("UpdateGoalCorrected"))
	}
	if got := h.store.stagedVersions(); len(got) != 0 {
		t.Fatalf("staged = %v, want nothing staged for an unreachable goal", got)
	}
}

// Flavor mix: first flavor wins, the loop proceeds within it.
func TestMixedPartitionKeepsFirstFlavor(t *testing.T) {
	h := newUpdateHarness(t, "stage", "6.18.48-simplek8s-202501010000 (amd64)",
		map[string]string{"simplek8s.org/next-kernel": "202501010000"},
		[]string{"202501010000", "202601010000"})
	h.store.setFlavor("202501010000", "rpi4")
	h.store.setFlavor("202601010000", "x86-64")
	h.repo.set(t, map[string][]byte{
		"simplek8s.202502020000.rpi4.efi.zst":   []byte("rpi4-kernel"),
		"simplek8s.202503030000.x86-64.efi.zst": []byte("x86-kernel"),
	})
	h.tick()
	// fake Kernels() preserves insertion order: rpi4 first → flavor rpi4.
	if got := h.store.stagedVersions(); len(got) != 1 || got[0] != "202502020000" {
		t.Fatalf("staged = %v, want the first flavor's release", got)
	}
	if got := h.nextKernel(); got != "202502020000" {
		t.Fatalf("next-kernel = %q, want anchored", got)
	}
}
