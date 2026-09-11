// Update annotations (PLAN-M2 3.4/3.8): orthogonal to the four reboot
// annotations. next-kernel is the permanent per-node boot intent
// (source of truth; == running is quiescent); update-url is an
// operator-only per-node release-repo override; reboot-eligible is a
// transient plan trigger the local pod sets on a fresh full-mode stage
// and the leader clears once the node is quiescent. (Design note:
// PLAN-M2 3.4 names exactly two; reboot-eligible is the M4 third.)
package nodestate

import (
	"regexp"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

const (
	AnnNextKernel      = "simplek8s.org/next-kernel"
	AnnUpdateURL       = "simplek8s.org/update-url"
	AnnRebootEligible  = "simplek8s.org/reboot-eligible"
	AnnUpdateLastCheck = "simplek8s.org/update-last-check"
)

// versionRe validates a next-kernel value: a release timestamp (digits).
var versionRe = regexp.MustCompile(`^[0-9]{6,20}$`)

// UpdateInfo is the parsed view of a node's update annotations.
type UpdateInfo struct {
	NextKernelPresent      bool
	NextKernel             string // "" when absent
	NextKernelParseErr     string // non-empty when present but not a valid version
	UpdateURL              string // "" when absent
	RebootEligiblePresent  bool
	RebootEligible         string // "" when absent
	RebootEligibleParseErr string // non-empty when present but not a valid version
	// LastCheck is the raw newest-checked-occurrence value
	// (RFC3339 UTC occurrence start, PLAN.md §3.4); LastCheckPresent
	// reports mere presence — parsing is the writer's job, and an
	// unparseable value reads as absent.
	LastCheckPresent bool
	LastCheck        string // "" when absent
}

// ParseUpdate decodes the update annotations tolerantly (a corrupt value
// never panics; it renders as present-but-unusable).
func ParseUpdate(annotations map[string]string) *UpdateInfo {
	ui := &UpdateInfo{}
	if raw, ok := annotations[AnnNextKernel]; ok {
		ui.NextKernelPresent = true
		ui.NextKernel = raw
		if !versionRe.MatchString(raw) {
			ui.NextKernelParseErr = "invalid version " + raw
		}
	}
	ui.UpdateURL = annotations[AnnUpdateURL]
	if raw, ok := annotations[AnnUpdateLastCheck]; ok {
		ui.LastCheckPresent = true
		ui.LastCheck = raw
	}
	if raw, ok := annotations[AnnRebootEligible]; ok {
		ui.RebootEligiblePresent = true
		ui.RebootEligible = raw
		if !versionRe.MatchString(raw) {
			ui.RebootEligibleParseErr = "invalid version " + raw
		}
	}
	return ui
}

// NextKernelPatch is the merge-patch section setting next-kernel to
// version (always carrying the fresh resourceVersion).
func NextKernelPatch(rv, version string) map[string]any {
	return annotationsPatch(rv, map[string]any{AnnNextKernel: version})
}

// NextKernelBuild returns a BuildFunc that sets next-kernel to version
// only when precond holds on the FRESH node (writer discipline,
// PLAN-M2 3.5: every controller write is a conditional RMW; a
// concurrent operator edit is re-evaluated, never clobbered).
func NextKernelBuild(version string, precond func(ui *UpdateInfo) bool) BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		if !precond(ParseUpdate(node.Metadata.Annotations)) {
			return nil, false
		}
		return NextKernelPatch(node.Metadata.ResourceVersion, version), true
	}
}

// NextKernelEligiblePatch is the merge-patch section setting next-kernel
// AND reboot-eligible to version in one atomic patch (fresh full-mode
// stage: the anchor and the plan trigger travel together, PLAN-M2 3.8).
func NextKernelEligiblePatch(rv, version string) map[string]any {
	return annotationsPatch(rv, map[string]any{
		AnnNextKernel:     version,
		AnnRebootEligible: version,
	})
}

// NextKernelEligibleBuild returns a BuildFunc that anchors next-kernel to
// version and sets reboot-eligible to version in the same conditional
// patch, only when precond holds on the fresh node. Used by the local
// pod on a fresh full-mode stage.
func NextKernelEligibleBuild(version string, precond func(ui *UpdateInfo) bool) BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		if !precond(ParseUpdate(node.Metadata.Annotations)) {
			return nil, false
		}
		return NextKernelEligiblePatch(node.Metadata.ResourceVersion, version), true
	}
}

// ClearRebootEligiblePatch deletes the reboot-eligible annotation (the
// leader's idempotent clear once a node is quiescent, PLAN-M2 3.8).
func ClearRebootEligiblePatch(rv string) map[string]any {
	return annotationsPatch(rv, map[string]any{AnnRebootEligible: nil})
}

// PrecondEligibleQuiescent: the reboot-eligible annotation is present and
// the node is quiescent (next-kernel present and == running) — i.e. it
// has settled onto its target and no longer needs the plan trigger.
func PrecondEligibleQuiescent(running string) func(*UpdateInfo) bool {
	return func(ui *UpdateInfo) bool {
		return ui.RebootEligiblePresent &&
			ui.NextKernelPresent && ui.NextKernelParseErr == "" &&
			ui.NextKernel == running
	}
}

// ClearRebootEligibleBuild returns a BuildFunc that deletes
// reboot-eligible only when the node is quiescent on a fresh re-read
// (the leader's idempotent clear; a node still pending its reboot —
// next-kernel != running — is never cleared).
func ClearRebootEligibleBuild(running string) BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		if !PrecondEligibleQuiescent(running)(ParseUpdate(node.Metadata.Annotations)) {
			return nil, false
		}
		return ClearRebootEligiblePatch(node.Metadata.ResourceVersion), true
	}
}

// UpdateLastCheckPatch is the merge-patch section claiming occurrence
// occ (RFC3339 UTC) as checked.
func UpdateLastCheckPatch(rv, occ string) map[string]any {
	return annotationsPatch(rv, map[string]any{AnnUpdateLastCheck: occ})
}

// UpdateLastCheckBuild returns a BuildFunc that claims occ only when
// the FRESH stored value is absent, unparseable, or older than occ
// (the per-occurrence claim, PLAN.md §3.4 decision 21: a concurrent
// claim for occ or newer wins, never clobbered).
func UpdateLastCheckBuild(occ time.Time) BuildFunc {
	occStr := occ.UTC().Format(time.RFC3339)
	return func(node *kube.Node) (map[string]any, bool) {
		if raw, ok := node.Metadata.Annotations[AnnUpdateLastCheck]; ok {
			if t, err := time.Parse(time.RFC3339, raw); err == nil && !t.Before(occ) {
				return nil, false
			}
		}
		return UpdateLastCheckPatch(node.Metadata.ResourceVersion, occStr), true
	}
}

// PrecondAbsent: bootstrap only (the annotation is absent).
func PrecondAbsent(ui *UpdateInfo) bool { return !ui.NextKernelPresent }

// PrecondQuiescent: staging anchor (absent, or the node already boots
// what it reports running — never over a held/re-pinned value).
func PrecondQuiescent(running string) func(*UpdateInfo) bool {
	return func(ui *UpdateInfo) bool {
		return !ui.NextKernelPresent || ui.NextKernel == running
	}
}

// PrecondValue: reset (the value is still exactly the given version).
func PrecondValue(v string) func(*UpdateInfo) bool {
	return func(ui *UpdateInfo) bool { return ui.NextKernelPresent && ui.NextKernel == v }
}

// CorrectGoalBuild returns a BuildFunc that corrects next-kernel to
// the safe state (PLAN.md §3.10): set to target, or delete the
// annotation when target is "". It lands only when the fresh value is
// still exactly badVal — a concurrent operator edit is re-evaluated,
// never clobbered.
func CorrectGoalBuild(badVal, target string) BuildFunc {
	return func(node *kube.Node) (map[string]any, bool) {
		ui := ParseUpdate(node.Metadata.Annotations)
		if !ui.NextKernelPresent || ui.NextKernel != badVal {
			return nil, false
		}
		if target == "" {
			return annotationsPatch(node.Metadata.ResourceVersion, map[string]any{AnnNextKernel: nil}), true
		}
		return NextKernelPatch(node.Metadata.ResourceVersion, target), true
	}
}
