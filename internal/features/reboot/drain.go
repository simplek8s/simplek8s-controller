package reboot

import (
	"context"
	"fmt"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/nodestate"
)

// drainResult is the outcome of one drain cycle for a node.
type drainResult int

const (
	// drainPending: evictable pods remain; retry next cycle.
	drainPending drainResult = iota
	// drainDone: no evictable pods remain; transition to rebooting.
	drainDone
	// drainFailed: timeout or force-delete error; transition to failed.
	drainFailed
)

// runDrain performs one drain cycle for a draining node (PLAN 3.5 step 4).
// It returns the result and, for drainFailed, the error text.
func (f *Feature) runDrain(ctx context.Context, node *kube.Node, st *nodestate.NodeState, allPods []kube.Pod) (drainResult, string) {
	now := f.cfg.Now()
	force := forceOf(st)
	if now.Sub(st.State.Since) >= f.cfg.DrainTimeout {
		return drainFailed, fmt.Sprintf("drain timed out after %s", f.cfg.DrainTimeout)
	}

	var toEvict []kube.Pod
	for i := range allPods {
		p := &allPods[i]
		if p.Spec.NodeName != node.Metadata.Name {
			continue
		}
		if evictable(p) {
			toEvict = append(toEvict, *p)
		}
	}
	if len(toEvict) == 0 {
		return drainDone, ""
	}

	for i := range toEvict {
		p := &toEvict[i]
		if p.Metadata.Terminating() {
			continue // already going; it still counts until it disappears
		}
		if unmanaged(p) && !force {
			continue // blocks the drain until timeout (operator window)
		}
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		f.mu.Lock()
		ds := f.denied[key]
		backoff := denyBackoff(ds)
		f.mu.Unlock()
		if ds != nil && now.Sub(ds.lastAttempt) < backoff {
			continue // PDB-denied recently: backoff (decision 19)
		}
		err := f.kube.Evict(ctx, p.Metadata.Namespace, p.Metadata.Name)
		f.mu.Lock()
		if kube.IsTooManyRequests(err) {
			if ds == nil {
				ds = &denyState{}
				f.denied[key] = ds
			}
			ds.count++
			ds.lastAttempt = now
		} else if err == nil || kube.IsNotFound(err) {
			delete(f.denied, key)
		}
		f.mu.Unlock()
	}

	if force {
		// After eviction attempts, DELETE remaining evictable pods
		// (including unmanaged). Never removes finalizers (3.5).
		for i := range toEvict {
			p := &toEvict[i]
			if p.Metadata.Terminating() {
				continue
			}
			err := f.kube.DeletePod(ctx, p.Metadata.Namespace, p.Metadata.Name)
			if err != nil && !kube.IsNotFound(err) {
				return drainFailed, fmt.Sprintf("force-delete of %s/%s failed: %v", p.Metadata.Namespace, p.Metadata.Name, err)
			}
		}
	}
	return drainPending, ""
}

// denyBackoff grows 5s * 2^(count-1), capped at 60s (decision 19).
func denyBackoff(ds *denyState) time.Duration {
	if ds == nil || ds.count == 0 {
		return 0
	}
	b := 5 * time.Second
	for i := 1; i < ds.count; i++ {
		b *= 2
		if b >= 60*time.Second {
			return 60 * time.Second
		}
	}
	return b
}

func forceOf(st *nodestate.NodeState) bool {
	if st.Request == nil || !st.Request.Present || st.Request.ParseError != "" {
		return false
	}
	return st.Request.Force
}
