package update

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
)

func planStoreClient(t *testing.T) (*kubetest.FakeAPI, *plansStore) {
	t.Helper()
	fake := kubetest.NewFakeAPI()
	t.Cleanup(fake.Close)
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	c, err := fake.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return fake, newPlansStore(c, "default", "simplek8s-update-plans")
}

// TestPlansLoadAbsentIsEmpty: a missing ConfigMap loads as an empty map.
func TestPlansLoadAbsentIsEmpty(t *testing.T) {
	_, s := planStoreClient(t)
	m, err := s.load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("loaded %d plans from an absent ConfigMap, want 0", len(m))
	}
}

// TestPlansSaveCreatesWhenAbsent: saving with no ConfigMap creates it.
func TestPlansSaveCreatesWhenAbsent(t *testing.T) {
	fake, s := planStoreClient(t)
	plans := map[string]PlanEntry{"202608291203": {StartedAt: "t0", Nodes: []string{"w1"}}}
	if err := s.save(context.Background(), plans); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := fake.ConfigMapData("default", "simplek8s-update-plans")
	if got == nil {
		t.Fatal("ConfigMap not created")
	}
	var back map[string]PlanEntry
	if err := json.Unmarshal([]byte(got["plans"]), &back); err != nil {
		t.Fatalf("plans payload invalid: %v", err)
	}
	if len(back) != 1 || back["202608291203"].Nodes[0] != "w1" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

// TestPlansSavePreservesForeignKeys: non-plan data keys survive a write.
func TestPlansSavePreservesForeignKeys(t *testing.T) {
	fake, s := planStoreClient(t)
	fake.SetConfigMap("default", "simplek8s-update-plans", map[string]string{"owner": "someone"})
	if err := s.save(context.Background(), map[string]PlanEntry{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	data := fake.ConfigMapData("default", "simplek8s-update-plans")
	if data["owner"] != "someone" {
		t.Fatalf("foreign key lost: %+v", data)
	}
	if data["plans"] != "{}" {
		t.Fatalf("plans key = %q, want {}", data["plans"])
	}
}

// TestPlansSaveRetriesOnConflict: a 409 (stale resourceVersion) is
// re-read and re-applied; the write lands on the second attempt.
func TestPlansSaveRetriesOnConflict(t *testing.T) {
	fake, s := planStoreClient(t)
	fake.SetConfigMap("default", "simplek8s-update-plans", map[string]string{"plans": "{}"})
	fake.ConfigMapConflicts = 1 // first PUT conflicts, second succeeds
	if err := s.save(context.Background(), map[string]PlanEntry{"V1": {Nodes: []string{"w1"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	data := fake.ConfigMapData("default", "simplek8s-update-plans")
	if data["plans"] == "{}" {
		t.Fatal("plans not rewritten after the conflict")
	}
}

// TestPlansLoadCorruptIsAnError: an unparseable plans payload is an error
// (the leader must not act on plan state it cannot read).
func TestPlansLoadCorruptIsAnError(t *testing.T) {
	fake, s := planStoreClient(t)
	fake.SetConfigMap("default", "simplek8s-update-plans", map[string]string{"plans": "{not json"})
	if _, err := s.load(context.Background()); err == nil {
		t.Fatal("load of a corrupt plans payload must fail")
	}
}
