package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

// Index files inside the release repo (PLAN-M2 3.6).
const (
	IndexFile      = "SHA256SUMS"
	IndexSignature = "SHA256SUMS.gpg"
)

// CheckResult is the outcome of one verified check for one node.
type CheckResult struct {
	URL       string
	Latest    string // newest ts for this node's arch in the verified index
	Running   string // ts from nodeInfo.kernelVersion
	Available bool   // Latest newer than Running
}

// Check performs the per-node release check (PLAN-M2 3.6): fetch the
// index and its detached signature, verify the signature against the
// resolved keyring (mandatory), map the node's architecture, and
// compute the newest available release. Any failure (network, GPG,
// parse, unsupported arch) is an error; the caller applies "no state
// change, rate-limited log + event".
func (f *Feature) Check(ctx context.Context, node *kube.Node, repoURL string) (CheckResult, error) {
	res := CheckResult{URL: repoURL}
	if repoURL == "" {
		return res, fmt.Errorf("no release repo URL")
	}

	base := strings.TrimSuffix(repoURL, "/")
	indexBytes, err := httpGet(ctx, f.http, base+"/"+IndexFile)
	if err != nil {
		return res, fmt.Errorf("fetch %s: %w", IndexFile, err)
	}
	sigBytes, err := httpGet(ctx, f.http, base+"/"+IndexSignature)
	if err != nil {
		return res, fmt.Errorf("fetch %s: %w", IndexSignature, err)
	}

	keyringPath, err := ResolveKeyring(f.cfg.CustomKeyring, f.cfg.EmbeddedKeyring)
	if err != nil {
		return res, err
	}
	keyring, err := LoadKeyring(keyringPath)
	if err != nil {
		return res, err
	}
	if _, err := VerifyIndex(keyring, indexBytes, sigBytes); err != nil {
		return res, err
	}

	sums, err := ParseIndex(indexBytes)
	if err != nil {
		return res, err
	}

	arch, ok := MapArch(node.Status.NodeInfo.Architecture)
	if !ok {
		return res, fmt.Errorf("unsupported node architecture %q", node.Status.NodeInfo.Architecture)
	}
	for file := range sums {
		ts, fileArch, isKernel := ParseKernelRelease(file)
		if !isKernel || fileArch != arch {
			continue
		}
		if res.Latest == "" || NewerTS(ts, res.Latest) {
			res.Latest = ts
		}
	}

	res.Running = RunningVersion(node.Status.NodeInfo.KernelVersion)
	if res.Latest != "" {
		res.Available = res.Running == "" || NewerTS(res.Latest, res.Running)
	}
	return res, nil
}

// httpGet downloads one file with a bounded size (index + signature are
// small; the bound guards against a hostile repo).
func httpGet(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return nil, err
	}
	return b, nil
}

const maxIndexBytes = 8 * 1024 * 1024
