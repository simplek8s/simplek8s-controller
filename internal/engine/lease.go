package engine

import (
	"context"
	"sync"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// Leader election constants (PLAN 3.10): renew every 10 s, takeover when
// the lease's renewTime is stale for more than 30 s.
const (
	LeaseRenewInterval = 10 * time.Second
	LeaseStaleAfter    = 30 * time.Second
)

// LeaseName is the global controller leader lease (generic: it leads the
// controller, not the reboot feature — PLAN 3.1/decision 5).
const LeaseName = "simplek8s-controller-leader"

// LeaseState is the outcome of a lease sync.
type LeaseState int

const (
	// LeaseLeader: this pod holds the lease with a fresh renewal.
	LeaseLeader LeaseState = iota
	// LeaseNotLeader: another holder renewed recently.
	LeaseNotLeader
	// LeaseNoValidLeader: the lease is absent or stale and this pod did
	// not acquire it this cycle.
	LeaseNoValidLeader
)

// Leaser implements the coordination.k8s.io/v1 Lease protocol
// (create / conditional renew / conditional acquire). A Lease is
// coordination, not hard fencing: CanDecide/VerifyOwnership add the
// mitigation required by PLAN 3.1/decision 12.
type Leaser struct {
	c        *kube.Client
	ns       string
	name     string
	identity string
	now      func() time.Time

	mu          sync.Mutex
	lastRenewed time.Time
	amLeader    bool
}

// NewLeaser builds a Leaser. identity is "<pod-name>/<pod-uid>".
func NewLeaser(c *kube.Client, ns, name, identity string, now func() time.Time) *Leaser {
	return &Leaser{c: c, ns: ns, name: name, identity: identity, now: now}
}

// Sync tries to renew or acquire the lease and returns the lease state.
func (l *Leaser) Sync(ctx context.Context) LeaseState {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	lease, err := l.c.GetLease(ctx, l.ns, l.name)
	if err != nil && !kube.IsNotFound(err) {
		// API error: keep previous belief, deadline handles staleness
		return l.belief(now)
	}

	if err == nil && lease.Spec.HolderIdentity == l.identity {
		// We hold the lease: renew at most once per renew interval.
		if now.Sub(l.lastRenewed) < LeaseRenewInterval {
			l.amLeader = true
			return l.belief(now)
		}
		t := kube.MicroTime(now)
		lease.Spec.RenewTime = &t
		if err := l.c.UpdateLease(ctx, lease); err == nil {
			l.lastRenewed = now
			l.amLeader = true
			return LeaseLeader
		} else if !kube.IsConflict(err) {
			return l.belief(now)
		}
		return l.classify(ctx, now)
	}

	if err == nil {
		// Foreign holder: takeover only when stale for > 30 s.
		if freshHolder(lease, now) {
			l.amLeader = false
			return LeaseNotLeader
		}
		t := kube.MicroTime(now)
		lease.Spec.HolderIdentity = l.identity
		lease.Spec.RenewTime = &t
		if err := l.c.UpdateLease(ctx, lease); err == nil {
			l.lastRenewed = now
			l.amLeader = true
			return LeaseLeader
		} else if !kube.IsConflict(err) {
			return LeaseNoValidLeader
		}
		return l.classify(ctx, now)
	}

	// Absent: create.
	t := kube.MicroTime(now)
	le := &kube.Lease{}
	le.Metadata.Name = l.name
	le.Metadata.Namespace = l.ns
	le.Spec.HolderIdentity = l.identity
	le.Spec.RenewTime = &t
	if err := l.c.CreateLease(ctx, le); err == nil {
		l.lastRenewed = now
		l.amLeader = true
		return LeaseLeader
	} else if !kube.IsConflict(err) {
		l.amLeader = false
		return LeaseNoValidLeader
	}
	return l.classify(ctx, now)
}

// freshHolder reports whether the lease holder renewed within the stale
// window.
func freshHolder(lease *kube.Lease, now time.Time) bool {
	return lease.Spec.RenewTime != nil && now.Sub(lease.Spec.RenewTime.Time()) <= LeaseStaleAfter
}

// classify re-reads the lease after a conflict and reports the current
// state deterministically.
func (l *Leaser) classify(ctx context.Context, now time.Time) LeaseState {
	lease, err := l.c.GetLease(ctx, l.ns, l.name)
	if err != nil {
		l.amLeader = false
		return LeaseNoValidLeader
	}
	if lease.Spec.HolderIdentity == l.identity && freshHolder(lease, now) {
		l.amLeader = true
		l.lastRenewed = now
		return LeaseLeader
	}
	if lease.Spec.HolderIdentity != l.identity && freshHolder(lease, now) {
		l.amLeader = false
		return LeaseNotLeader
	}
	l.amLeader = false
	return LeaseNoValidLeader
}

// belief reports the local belief while honoring the renewal deadline:
// no decisions are made once the local renewal deadline expires (3.1).
func (l *Leaser) belief(now time.Time) LeaseState {
	if l.amLeader && now.Sub(l.lastRenewed) <= LeaseStaleAfter {
		return LeaseLeader
	}
	if l.amLeader {
		// we believed we were leader but missed renewals: stop deciding
		return LeaseNoValidLeader
	}
	return LeaseNoValidLeader
}

// Leader reports the current local belief (freshness included).
func (l *Leaser) Leader() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	return l.amLeader && now.Sub(l.lastRenewed) <= LeaseStaleAfter
}

// CanDecide reports whether the local renewal deadline has not expired
// (PLAN 3.1: the orchestrator stops making lifecycle decisions then).
func (l *Leaser) CanDecide() bool { return l.Leader() }

// VerifyOwnership re-reads the lease and verifies holderIdentity before a
// lifecycle transition patch (decision 12). It returns false when the
// lease is missing or not held by this pod.
func (l *Leaser) VerifyOwnership(ctx context.Context) bool {
	lease, err := l.c.GetLease(ctx, l.ns, l.name)
	if err != nil {
		return false
	}
	return lease.Spec.HolderIdentity == l.identity
}
