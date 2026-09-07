// Update annotations (PLAN-M2 3.4): exactly two, orthogonal to the four
// reboot annotations. next-kernel is the permanent per-node boot intent
// (source of truth; == running is quiescent); update-url is an
// operator-only per-node release-repo override.
package nodestate

import (
	"regexp"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

const (
	AnnNextKernel = "simplek8s.org/next-kernel"
	AnnUpdateURL  = "simplek8s.org/update-url"
)

// versionRe validates a next-kernel value: a release timestamp (digits).
var versionRe = regexp.MustCompile(`^[0-9]{6,20}$`)

// UpdateInfo is the parsed view of a node's update annotations.
type UpdateInfo struct {
	NextKernelPresent  bool
	NextKernel         string // "" when absent
	NextKernelParseErr string // non-empty when present but not a valid version
	UpdateURL          string // "" when absent
}

// ParseUpdate decodes the two update annotations tolerantly (a corrupt
// value never panics; it renders as present-but-unusable).
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
