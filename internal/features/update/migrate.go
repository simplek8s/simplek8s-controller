package update

import (
	"context"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// Self-cleaning migration (PLAN.md §3.4 decisions 15, 26): the M2 plan
// layer is gone, so its leftovers are deleted best-effort — the
// leader-owned plan ConfigMap (once per leadership acquisition until
// it succeeds) and the local pod's reboot-eligible marker (one-shot at
// pod start). Both remember completion for the pod lifetime.

// cleanLeftoverPlans deletes a leftover M2 plan ConfigMap. NotFound
// means done (remembered); any other error Warns and retries on the
// next leadership acquisition — a once-per-lifetime attempt could be
// lost to the rollout race in which the new RBAC role has not
// propagated yet. Empty namespace/name disables the cleanup.
func (f *Feature) cleanLeftoverPlans(ctx context.Context) {
	ns, name := f.cfg.PlanConfigMapNamespace, f.cfg.PlanConfigMapName
	done := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.plansCleaned = true
	}
	if ns == "" || name == "" {
		done()
		return
	}
	err := f.kube.DeleteConfigMap(ctx, ns, name)
	switch {
	case err == nil:
		f.log.Info("update: deleted leftover plan ConfigMap", "configmap", ns+"/"+name)
		done()
	case kube.IsNotFound(err):
		done()
	default:
		f.log.Warn("update: leftover plan ConfigMap delete failed; retrying on next leadership acquisition",
			"configmap", ns+"/"+name, "err", err)
	}
}

// cleanLeftoverEligible deletes a stale M2 reboot-eligible marker on
// the local pod's own node, once per pod lifetime. Best-effort: a
// missing node is debug-logged (the pod cannot outlive it for long).
func (f *Feature) cleanLeftoverEligible(ctx context.Context) {
	name := f.cfg.NodeName
	err := nodestate.PatchTransition(ctx, f.kube, name, 3, func(fresh *kube.Node) (map[string]any, bool) {
		if !nodestate.ParseUpdate(fresh.Metadata.Annotations).RebootEligiblePresent {
			return nil, false
		}
		return nodestate.ClearRebootEligiblePatch(fresh.Metadata.ResourceVersion), true
	})
	switch {
	case err == nil:
		f.log.Info("update: deleted leftover reboot-eligible marker", "node", name)
	case nodestate.ErrAbort == err:
		// Already absent.
	case kube.IsNotFound(err):
		f.log.Debug("update: own node not found for marker cleanup")
	default:
		f.log.Warn("update: leftover reboot-eligible delete failed", "node", name, "err", err)
	}
}
