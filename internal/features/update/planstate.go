package update

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// PlanEntry is one active (or canceling) update plan, keyed by its version
// in the leader-owned plan ConfigMap (PLAN-M2 3.8). Nodes is the member
// set frozen at plan start; it is the durable source of truth across
// leader restarts (takeover resumes from it, not from node annotations).
type PlanEntry struct {
	StartedAt string   `json:"startedAt"`
	Nodes     []string `json:"nodes"`
	Canceling bool     `json:"canceling,omitempty"`
}

// plansKey is the ConfigMap data key holding the JSON plans map.
const plansKey = "plans"

// plansStore reads and writes the leader-owned plan ConfigMap
// (simplek8s-update-plans). The leader is the only writer; the standby
// reads it at takeover. It is written only on plan transitions (a few
// writes per plan), never on the heartbeat (PLAN-M2 3.8/3.9).
type plansStore struct {
	kube *kube.Client
	ns   string
	name string
}

func newPlansStore(c *kube.Client, ns, name string) *plansStore {
	return &plansStore{kube: c, ns: ns, name: name}
}

// load reads the current plans map. An absent ConfigMap (404) or a
// missing/empty plans key yields an empty map. An unparseable plans
// payload is an error: the leader must not act on plan state it cannot
// read (dropping it would strand in-flight members or double-admit).
func (s *plansStore) load(ctx context.Context) (map[string]PlanEntry, error) {
	cm, err := s.kube.GetConfigMap(ctx, s.ns, s.name)
	if err != nil {
		if kube.IsNotFound(err) {
			return map[string]PlanEntry{}, nil
		}
		return nil, err
	}
	raw, ok := cm.Data[plansKey]
	if !ok || raw == "" {
		return map[string]PlanEntry{}, nil
	}
	var m map[string]PlanEntry
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("update: corrupt plan ConfigMap %s/%s: %w", s.ns, s.name, err)
	}
	if m == nil {
		m = map[string]PlanEntry{}
	}
	return m, nil
}

// save rewrites the ConfigMap with the given plans, carrying a fresh
// resourceVersion. It re-reads on 409 (optimistic concurrency) and
// re-applies up to 3 times; the in-memory plan state is authoritative
// within the cycle and re-derived on the next, so a failed write is
// surfaced (logged) rather than retried indefinitely. Foreign data keys
// are preserved.
func (s *plansStore) save(ctx context.Context, plans map[string]PlanEntry) error {
	for attempt := 0; attempt < 3; attempt++ {
		cm, err := s.kube.GetConfigMap(ctx, s.ns, s.name)
		var create bool
		switch {
		case err == nil:
			// existing: keep the fresh resourceVersion from the read.
		case kube.IsNotFound(err):
			cm = &kube.ConfigMap{}
			cm.Metadata.Name = s.name
			cm.Metadata.Namespace = s.ns
			create = true
		default:
			return err
		}
		payload, err := json.Marshal(plans)
		if err != nil {
			return err
		}
		data := make(map[string]string, len(cm.Data)+1)
		for k, v := range cm.Data {
			if k != plansKey {
				data[k] = v
			}
		}
		data[plansKey] = string(payload)
		cm.Data = data

		if create {
			if err := s.kube.CreateConfigMap(ctx, cm); err != nil {
				if kube.IsConflict(err) {
					continue
				}
				return err
			}
			return nil
		}
		if err := s.kube.UpdateConfigMap(ctx, cm); err != nil {
			if kube.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("update: plan ConfigMap %s/%s still conflicting after retries", s.ns, s.name)
}
