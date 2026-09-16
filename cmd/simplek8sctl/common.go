package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/simplek8s/simplek8s-controller/internal/features/update"
)

// Default release channel (same default as the controller).
const defaultRepoURL = "https://dl.simplek8s.org/simplek8s/stable"

// expandRepoURL keeps the legacy short-channel expansion (dev |
// rolling | stable) and passes custom URLs through.
func expandRepoURL(u string) string {
	switch u {
	case "dev":
		return "https://dl.simplek8s.org/simplek8s/dev"
	case "rolling":
		return "https://dl.simplek8s.org/simplek8s/rolling"
	case "stable":
		return defaultRepoURL
	default:
		return u
	}
}

// repoBase trims one trailing slash for artifact URL joining.
func repoBase(u string) string {
	return strings.TrimSuffix(expandRepoURL(u), "/")
}

// requireRoot fails closed: every command mounts the boot partition.
func requireRoot(log *slog.Logger) int {
	if os.Geteuid() != 0 {
		log.Error("must run as root on the node itself")
		return exitMisuse
	}
	return exitOK
}

// openStore builds the physical store with CLI defaults (own
// mountpoint dir under the shared /run/simplek8s).
func openStore(log *slog.Logger) *update.PhysicalStore {
	return update.NewPhysicalStore(update.PhysicalStoreConfig{
		MountRoot: "/run/simplek8s/mnt",
		Log:       log,
	})
}

// osRelease returns the running kernel release for uname -r.
func osRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return update.RunningVersion(strings.TrimSpace(string(b)))
}

// deviceTree reads the board model + compatible (either of the two
// conventional paths; absent on x86-64).
func deviceTree() (model, compatible string) {
	model, _ = readFirst("/sys/firmware/devicetree/base/model", "/proc/device-tree/model")
	compatible, _ = readFirst("/sys/firmware/devicetree/base/compatible", "/proc/device-tree/compatible")
	return model, compatible
}

func readFirst(paths ...string) (string, error) {
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("none of %v readable", paths)
}

// detectFlavor resolves the board flavor from staged basenames,
// device-tree and build arch (PLAN-M6 D8/D11). ok=false is fail-closed
// (exit 2), never a guess.
func detectFlavor(kernels []string) (string, bool) {
	model, compatible := deviceTree()
	return update.DetectArchAuto(kernels, model, compatible, runtime.GOARCH)
}

// httpClient is the artifact/index client (redirects + timeout).
func httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// fetchIndex fetches + GPG-verifies the release index (or skips
// verification for --keyring /dev/null with a loud warning),
// returning the verified filename -> sha256 map.
func fetchIndex(ctx context.Context, log *slog.Logger, base, keyringPath string) (map[string]string, int) {
	sums, err := update.FetchVerifiedIndex(ctx, httpClient(), log, base, keyringPath, embeddedPubring)
	if err != nil {
		if keyringPath == "/dev/null" {
			log.Warn("GPG verification skipped (--keyring /dev/null): transport integrity only")
		}
		log.Error("release index check failed", "err", err)
		return nil, exitOperational
	}
	if keyringPath == "/dev/null" {
		log.Warn("GPG verification skipped (--keyring /dev/null): transport integrity only")
	}
	return sums, exitOK
}

// versionsBeforeAfter diffs staged ts lists (for purge/update reporting).
func versionsBeforeAfter(before, after []string) (deleted, kept []string) {
	afterSet := make(map[string]bool, len(after))
	for _, v := range after {
		afterSet[v] = true
	}
	for _, v := range before {
		if !afterSet[v] {
			deleted = append(deleted, v)
		}
	}
	return deleted, after
}
