package update

import (
	"context"
	"fmt"
	"strings"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// CheckResult is the outcome of one verified check for one node.
type CheckResult struct {
	URL       string
	Arch      string            // node's board flavor (x86-64/arm64/rpi4/rpi5); index filtered to it
	Latest    string            // newest ts for this node's flavor in the verified index
	Running   string            // ts from nodeInfo.kernelVersion
	Available bool              // Latest newer than Running
	Artifact  string            // filename of the latest kernel artifact
	Checksum  string            // sha256 of that artifact (verified index)
	Sums      map[string]string // the full verified index (filename -> sha256)
}

// Check performs the per-node release check (PLAN-M2 3.6): fetch the
// index and its detached signature, verify the signature against the
// resolved keyring (mandatory), and compute the newest available
// release **for the node's flavor** (PLAN-M5 §3.2). Any failure
// (network, GPG, parse) is an error; the caller applies "no state
// change, rate-limited log + event".
func (f *Feature) Check(ctx context.Context, node *kube.Node, repoURL, flavor string) (CheckResult, error) {
	res := CheckResult{URL: repoURL}
	if repoURL == "" {
		return res, fmt.Errorf("no release repo URL")
	}
	if flavor == "" {
		return res, fmt.Errorf("unresolved board flavor")
	}

	base := strings.TrimSuffix(repoURL, "/")
	indexBytes, err := updatecore.FetchBytes(ctx, f.http, base+"/"+updatecore.IndexFile)
	if err != nil {
		return res, fmt.Errorf("fetch %s: %w", updatecore.IndexFile, err)
	}
	sigBytes, err := updatecore.FetchBytes(ctx, f.http, base+"/"+updatecore.IndexSignature)
	if err != nil {
		return res, fmt.Errorf("fetch %s: %w", updatecore.IndexSignature, err)
	}

	keyringPath, err := updatecore.ResolveKeyring(f.cfg.CustomKeyring, f.cfg.EmbeddedKeyring)
	if err != nil {
		return res, err
	}
	keyring, err := updatecore.LoadKeyring(keyringPath)
	if err != nil {
		return res, err
	}
	if _, err := updatecore.VerifyIndex(keyring, indexBytes, sigBytes); err != nil {
		return res, err
	}

	sums, err := updatecore.ParseIndex(indexBytes)
	if err != nil {
		return res, err
	}

	arch := flavor
	res.Arch = arch
	res.Sums = sums
	for file := range sums {
		ts, fileArch, isKernel := updatecore.ParseKernelRelease(file)
		if !isKernel || fileArch != arch {
			continue
		}
		if res.Latest == "" || updatecore.NewerTS(ts, res.Latest) {
			res.Latest = ts
		}
	}

	res.Running = updatecore.RunningVersion(node.Status.NodeInfo.KernelVersion)
	if res.Latest != "" {
		res.Available = res.Running == "" || updatecore.NewerTS(res.Latest, res.Running)
		res.Artifact = updatecore.ArtifactFileName(res.Latest, arch)
		res.Checksum = sums[res.Artifact]
	}
	return res, nil
}
