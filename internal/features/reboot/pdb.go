package reboot

import (
	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// pdbBlocked evaluates, for target node N, every PDB whose namespace
// contains selected pods running on N (PLAN 3.7). It returns the
// "ns/pdb" names of the PDBs that block the drain. The in-memory
// pre-check and the server-side Eviction API can diverge; the API is
// the arbiter (decision: tests must cover deliberate divergence).
func pdbBlocked(node *kube.Node, pdbs []kube.PodDisruptionBudget, allPods []kube.Pod) []string {
	var blocked []string
	for i := range pdbs {
		pdb := &pdbs[i]
		if pdb.Spec.Selector == nil {
			continue
		}
		var selectedOnN, disruptable int
		for j := range allPods {
			p := &allPods[j]
			if p.Metadata.Namespace != pdb.Metadata.Namespace {
				continue
			}
			if !pdb.Spec.Selector.Matches(p.Metadata.Labels) {
				continue
			}
			if !p.Running() || p.Metadata.Terminating() {
				continue
			}
			if p.Spec.NodeName != node.Metadata.Name {
				continue
			}
			selectedOnN++
			if evictable(p) {
				disruptable++
			}
		}
		if selectedOnN == 0 {
			continue // PDB's namespace has no selected pods on N
		}
		if disruptable > 0 && int(pdb.Status.DisruptionsAllowed) < disruptable {
			blocked = append(blocked, pdb.Metadata.Namespace+"/"+pdb.Metadata.Name)
		}
	}
	return blocked
}

// evictable reports whether the drain would remove the pod (i.e. the pod
// is NOT on the drain skip list, 3.5 step 4). Unmanaged pods (no
// ownerReferences) are evictable: without force the drain blocks on
// them, with force they are deleted.
func evictable(p *kube.Pod) bool {
	if p.Metadata.Terminating() {
		return false
	}
	if isDaemonSetManaged(p) {
		return false
	}
	if isMirrorOfStatic(p) {
		return false
	}
	return true
}

// isDaemonSetManaged reports whether a DaemonSet owns the pod.
func isDaemonSetManaged(p *kube.Pod) bool {
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isMirrorOfStatic reports the kubernetes.io/config.mirror annotation
// plus an ownerReference of kind Node (the API objects of CP static
// pods).
func isMirrorOfStatic(p *kube.Pod) bool {
	if _, ok := p.Metadata.Annotations["kubernetes.io/config.mirror"]; !ok {
		return false
	}
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == "Node" {
			return true
		}
	}
	return false
}

// unmanaged reports whether no controller will recreate the pod (no
// ownerReferences). Such pods block the drain without force.
func unmanaged(p *kube.Pod) bool {
	return len(p.Metadata.OwnerReferences) == 0
}
