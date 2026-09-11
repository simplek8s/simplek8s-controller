package update

import (
	"context"
)

// flavorOf returns the node's resolved board flavor (PLAN-M5 §3.1),
// cached per pod lifetime. The first resolution scans the boot
// partition (one mount); afterwards no I/O. ok==false when unresolvable
// (scan error, empty or foreign-only partition): callers skip update
// work for the cycle (D2 fail-closed). A mid-life flavor mix is
// Warn-logged once, never adopted.
func (f *Feature) flavorOf(ctx context.Context) (string, bool) {
	f.mu.Lock()
	known, flavor := f.flavorKnown, f.flavor
	f.mu.Unlock()
	if known {
		return flavor, flavor != ""
	}
	if f.cfg.Store == nil {
		return "", false
	}
	files, err := f.cfg.Store.Kernels(ctx)
	if err != nil {
		f.log.Debug("update: flavor scan failed; retrying next cycle", "err", err)
		return "", false
	}
	flavor, ok := ResolveFlavor(files)
	if !ok {
		// Empty/foreign-only: fail closed but do NOT cache — a later
		// hand-staged file must heal without a pod restart (D2).
		// Warn once per unresolvable stretch (not per cycle).
		f.mu.Lock()
		warned := f.flavorWarned
		f.flavorWarned = true
		f.mu.Unlock()
		if !warned {
			f.log.Warn("update: board flavor unresolvable (empty or foreign-only partition); skipping update work until files appear")
		} else {
			f.log.Debug("update: board flavor still unresolvable; skipping update work")
		}
		return "", false
	}
	// Mixed-flavor sanity: more than one known flavor present.
	seen := map[string]bool{}
	for _, name := range files {
		if _, fa, ok := ParseStoredKernel(name); ok && flavorSet[fa] {
			seen[fa] = true
		}
	}
	f.mu.Lock()
	f.flavor, f.flavorKnown = flavor, true
	f.flavorWarned = false
	warned := f.mixWarned
	if len(seen) > 1 && !warned {
		f.mixWarned = true
	}
	f.mu.Unlock()
	if len(seen) > 1 && !warned {
		f.log.Warn("update: mixed board flavors on the partition; sticking to first", "flavor", flavor)
	} else {
		f.log.Info("update: board flavor resolved", "flavor", flavor)
	}
	return flavor, true
}
