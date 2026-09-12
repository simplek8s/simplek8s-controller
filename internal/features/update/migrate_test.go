package update

import (
	"testing"

	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

const stalePlansCM = "simplek8s-update-plans"

// withMigration wires the leftover plan-ConfigMap location into a plan
// harness feature (same package).
func withMigration(h *planHarness) {
	h.t.Helper()
	h.f.cfg.PlanConfigMapNamespace = "default"
	h.f.cfg.PlanConfigMapName = stalePlansCM
}

func plansCMPresent(h *planHarness) bool {
	h.t.Helper()
	return h.fake.ConfigMapData("default", stalePlansCM) != nil
}

// TestMigrationDeletesLeftoverPlansCM: a stale M2 plan ConfigMap is
// deleted on the first leadership, and remembered for the pod
// lifetime (no repeat delete).
func TestMigrationDeletesLeftoverPlansCM(t *testing.T) {
	h := newPlanHarness(t)
	withMigration(h)
	h.fake.SetConfigMap("default", stalePlansCM, map[string]string{"plans": "{}"})
	h.leader()
	h.orch()
	if plansCMPresent(h) {
		t.Fatal("leftover plan ConfigMap must be deleted on first leadership")
	}
	if !h.f.plansCleaned {
		t.Fatal("cleanup must be remembered for the pod lifetime")
	}
	h.orch() // second cycle: no repeat attempt, no error
	if plansCMPresent(h) {
		t.Fatal("ConfigMap must stay gone")
	}
}

// TestMigrationMissingPlansCMIsDone: no leftover means done (no warn,
// no retry state).
func TestMigrationMissingPlansCMIsDone(t *testing.T) {
	h := newPlanHarness(t)
	withMigration(h)
	h.leader()
	h.orch()
	if !h.f.plansCleaned {
		t.Fatal("missing ConfigMap must count as done")
	}
}

// TestMigrationRetriesOnNextAcquisition: a failed delete (e.g. the
// rollout race: RBAC role not yet propagated) Warns and retries once
// on the NEXT leadership acquisition — not every cycle.
func TestMigrationRetriesOnNextAcquisition(t *testing.T) {
	h := newPlanHarness(t)
	withMigration(h)
	h.fake.SetConfigMap("default", stalePlansCM, map[string]string{"plans": "{}"})
	h.fake.FailNext["configmaps/"+stalePlansCM] = 3 // all client attempts fail
	h.leader()
	h.orch() // attempt 1: fails
	if !plansCMPresent(h) {
		t.Fatal("ConfigMap must survive the failed attempt")
	}
	if h.f.plansCleaned {
		t.Fatal("failed cleanup must not be remembered")
	}
	h.orch() // same acquisition: no retry
	if h.fake.FailNext["configmaps/"+stalePlansCM] != 0 {
		t.Fatalf("FailNext = %d, want 0 consumed by exactly one attempt",
			h.fake.FailNext["configmaps/"+stalePlansCM])
	}
	if !plansCMPresent(h) {
		t.Fatal("ConfigMap must still be present (no same-cycle retry)")
	}
	// Simulate loss + re-acquisition: the edge re-arms the retry.
	h.f.wasLeader = false
	h.orch() // attempt 2: succeeds
	if plansCMPresent(h) {
		t.Fatal("ConfigMap must be deleted on the next acquisition")
	}
	if !h.f.plansCleaned {
		t.Fatal("successful cleanup must be remembered")
	}
}

// TestMigrationDeletesLeftoverEligibleOnce: the local pod deletes a
// stale M2 reboot-eligible marker at pod start (one-shot,
// best-effort), in every update mode.
func TestMigrationDeletesLeftoverEligibleOnce(t *testing.T) {
	h := newUpdateHarness(t, "6.18.48-simplek8s-202601010000 (amd64)",
		map[string]string{
			"simplek8s.org/next-kernel": "202601010000",
			nodestate.AnnRebootEligible: "202601010000",
		},
		[]string{"202601010000"})
	h.tick()
	if got := h.fake.NodeAnnotation("w1", nodestate.AnnRebootEligible); got != "" {
		t.Fatalf("reboot-eligible = %q, want deleted at pod start", got)
	}
	h.tick() // one-shot: still gone, no error
	if got := h.fake.NodeAnnotation("w1", nodestate.AnnRebootEligible); got != "" {
		t.Fatalf("reboot-eligible = %q after 2nd tick", got)
	}
}
