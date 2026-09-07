package update

// Partition space stats (PLAN-M2 3.7 step 3/6). statfs on the mounted
// partition; the "free" figure is the space available to an unprivileged
// writer (Bavail), matching the reference purge accounting.

import (
	"fmt"
	"syscall"
)

// PathInfo returns the total/free/used bytes of the filesystem at path.
func PathInfo(path string) (total, free, used uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	frsz := uint64(st.Frsize)
	reserved := uint64(st.Bfree) - uint64(st.Bavail)
	if reserved > uint64(st.Blocks) {
		reserved = uint64(st.Blocks)
	}
	total = frsz * (uint64(st.Blocks) - reserved)
	free = frsz * uint64(st.Bavail)
	used = total - free
	return total, free, used, nil
}
