#!/bin/sh
# Preflight for `make image` (PLAN.md §3.11): the foreign-arch `apk add`
# layer runs a foreign-arch shell, which needs qemu/binfmt_misc
# registered once on Linux. Without it, buildx fails deep in the build
# with a cryptic foreign-arch shell error — so the message comes from
# here instead.
set -eu
arch=$(uname -m)
case "$arch" in
	x86_64) need=qemu-aarch64; plat=arm64 ;;
	aarch64) need=qemu-x86_64; plat=amd64 ;;
	*)
		echo "make image: unsupported host arch $arch (want x86_64/aarch64)" >&2
		exit 1
		;;
esac
# Non-Linux (no binfmt_misc): Docker Desktop and CI runners emulate
# transparently; only warn.
if [ ! -d /proc/sys/fs/binfmt_misc ]; then
	echo "make image: no /proc/sys/fs/binfmt_misc (non-Linux?); assuming transparent emulation" >&2
	exit 0
fi
if [ -e "/proc/sys/fs/binfmt_misc/$need" ]; then
	exit 0
fi
cat >&2 <<EOF
make image: $need not registered under /proc/sys/fs/binfmt_misc — the
foreign-arch ($plat) build stage cannot run.
Fix (once): docker run --privileged --rm tonistiigi/binfmt --install $plat
EOF
exit 1
