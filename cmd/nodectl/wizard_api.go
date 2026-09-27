package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	updatecore "github.com/simplek8s/simplek8s-controller/internal/updatecore"
)

// Wizard read APIs (PLAN.md §3.16): install candidates and
// release versions. Both reuse the M8/M6 primitives; the
// release index is cached briefly (a fetch costs a download
// plus a GPG verification).

// indexCache memoizes the verified release index per base URL.
type indexCache struct {
	mu  sync.Mutex
	ttl time.Duration
	by  map[string]indexEntry
}

type indexEntry struct {
	at   time.Time
	sums map[string]string
}

var releaseIndex = &indexCache{ttl: 5 * time.Minute, by: map[string]indexEntry{}}

func cachedIndex(ctx context.Context, log *slog.Logger, baseURL string) (map[string]string, error) {
	releaseIndex.mu.Lock()
	if e, ok := releaseIndex.by[baseURL]; ok && time.Since(e.at) < releaseIndex.ttl {
		sums := e.sums
		releaseIndex.mu.Unlock()
		return sums, nil
	}
	releaseIndex.mu.Unlock()
	sums, code := fetchIndex(ctx, log, baseURL, "")
	if code != exitOK {
		return nil, fmt.Errorf("release index check failed")
	}
	releaseIndex.mu.Lock()
	defer releaseIndex.mu.Unlock()
	releaseIndex.by[baseURL] = indexEntry{at: time.Now(), sums: sums}
	return sums, nil
}

// wizardFlavor resolves this node's install flavor (same rule
// as the CLI: device-tree, else build arch).
func wizardFlavor() (string, bool) {
	model, compatible := deviceTree()
	return updatecore.InstallFlavor(model, compatible, runtime.GOARCH)
}

type versionInfo struct {
	TS   string `json:"ts"`
	File string `json:"file"`
}

func versionsHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		if channel == "" {
			channel = "stable"
		}
		baseURL := repoBase(channel)
		sums, err := cachedIndex(r.Context(), log, baseURL)
		if err != nil {
			log.Error("versions index failed", "err", err)
			apiError(w, http.StatusBadGateway, "release channel unreachable")
			return
		}
		flavor, ok := wizardFlavor()
		if !ok {
			apiError(w, http.StatusInternalServerError, "install flavor unresolvable")
			return
		}
		var out []versionInfo
		for file := range sums {
			if ts, arch, ok := updatecore.ParseImgRelease(file); ok && arch == flavor {
				out = append(out, versionInfo{TS: ts, File: file})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
		if out == nil {
			out = []versionInfo{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"flavor": flavor, "releases": out})
	}
}

// networkHandler reports the node's primary IP (the default
// route's interface address, like getty's \4) so the init
// form can prefill the advertise address.
func networkHandler(log *slog.Logger) http.HandlerFunc {
	_ = log
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"primaryIP": defaultEgressIP()})
	}
}

// defaultRouteInterface parses the interface behind the
// default route from /proc/net/route content (destination 0,
// flag RTF_GATEWAY).
func defaultRouteInterface(routes string) (string, bool) {
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] == "Iface" {
			continue
		}
		if f[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x2 == 0 {
			continue
		}
		return f[0], true
	}
	return "", false
}

// defaultEgressIP returns the first non-loopback IPv4 of the
// default-route interface ("" when indeterminate).
func defaultEgressIP() string {
	raw, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	iface, ok := defaultRouteInterface(string(raw))
	if !ok {
		return ""
	}
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return ""
	}
	addrs, err := link.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// diskInfo is one install candidate for the picker. The boot
// disk is listed disabled (never selectable); any disk with
// mounted filesystems is refused like the CLI does.
type diskInfo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"sizeBytes"`
	Model      string `json:"model,omitempty"`
	Mounted    bool   `json:"mounted"`
	Boot       bool   `json:"boot"`
	Selectable bool   `json:"selectable"`
	Reason     string `json:"reason"`
}

// diskModel returns a human identifier for the picker
// (best-effort, "" when absent): device/model plus the
// device/rev when present, e.g. "QEMU HARDDISK (rev 2.5+)".
// virtio-blk exposes no model file, only a vendor id
// ("0x1af4"), which still beats a bare /dev/vda. A bare
// /dev/vda tells the operator nothing; the identifier is the
// last cheap check before wiping the wrong disk.
func diskModel(base string) string {
	raw, err := os.ReadFile(wizardSysBlock + "/" + base + "/device/model")
	if err != nil {
		if v, verr := os.ReadFile(wizardSysBlock + "/" + base + "/device/vendor"); verr == nil {
			if vendor := strings.TrimSpace(string(v)); vendor != "" {
				if vendor == "0x1af4" {
					return "virtio 0x1af4"
				}
				return vendor
			}
		}
		return ""
	}
	model := strings.TrimSpace(string(raw))
	if model == "" {
		return ""
	}
	if rev, err := os.ReadFile(wizardSysBlock + "/" + base + "/device/rev"); err == nil {
		if rev := strings.TrimSpace(string(rev)); rev != "" {
			model += " (rev " + rev + ")"
		}
	}
	return model
}

// virtualDiskPrefixes are never install candidates.
func virtualDisk(base string) bool {
	for _, p := range []string{"loop", "ram", "dm-", "md", "zram", "fd", "sr", "nbd"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// rootDiskBase finds the sysfs base holding the mounted rootfs
// (the running boot disk): mountinfo major:minor of "/" matched
// against every block node under the sysfs root.
func rootDiskBase() string {
	raw, err := os.ReadFile(wizardMountinfo)
	if err != nil {
		return ""
	}
	maj, min, ok := mountinfoRootDev(string(raw))
	if !ok {
		return ""
	}
	want := [2]int{maj, min}
	bases, _ := filepath.Glob(wizardSysBlock + "/*")
	for _, b := range bases {
		base := filepath.Base(b)
		if devNoOf("/dev/"+base) == want {
			return base
		}
		parts, _ := filepath.Glob(wizardSysBlock + "/" + base + "/" + base + "*")
		for _, p := range parts {
			if devNoOf("/dev/"+filepath.Base(p)) == want {
				return base
			}
		}
	}
	return ""
}

// mountinfoRootDev parses the major:minor of the "/" mount
// from mountinfo content (field 3, mount point field 5).
func mountinfoRootDev(mountinfo string) (maj, min int, ok bool) {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if fields[4] != "/" {
			continue
		}
		return splitDevNo(fields[2])
	}
	return 0, 0, false
}

func disksHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mounts, err := readMountTable()
		if err != nil {
			log.Error("reading mounts failed", "err", err)
			apiError(w, http.StatusInternalServerError, "cannot list disks")
			return
		}
		boot := rootDiskBase()
		entries, err := os.ReadDir(wizardSysBlock)
		if err != nil {
			log.Error("reading sysfs failed", "err", err)
			apiError(w, http.StatusInternalServerError, "cannot list disks")
			return
		}
		out := []diskInfo{}
		for _, e := range entries {
			base := e.Name()
			if virtualDisk(base) {
				continue
			}
			target := "/dev/" + base
			// Whole disks only (partitions nest one level deeper).
			if st, err := os.Stat(wizardSysBlock + "/" + base); err != nil || !st.IsDir() {
				continue
			}
			size, err := installTargetSize(base)
			if err != nil {
				continue
			}
			d := diskInfo{Name: base, Path: target, SizeBytes: size, Model: diskModel(base)}
			d.Mounted = installTargetMounted(mounts, target, base)
			d.Boot = boot != "" && boot == base
			d.Selectable = true
			switch {
			case d.Boot:
				d.Selectable = false
				d.Reason = "running boot device"
			case d.Mounted:
				d.Selectable = false
				d.Reason = "has mounted filesystems"
			case size < installSizeFloor:
				d.Selectable = false
				d.Reason = "smaller than 1GiB"
			}
			out = append(out, d)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"disks": out})
	}
}
