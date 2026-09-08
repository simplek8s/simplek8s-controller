# AGENTS — operational notes

Durable notes to avoid repeating mistakes when working with the SimpleK8s
test VMs from this repo.

## SimpleK8s VM operations

- **SimpleK8s does NOT mount `/boot`** — there is no `/boot` on a running
  node. The boot partition is `/dev/vda1` (vfat, LABEL `EFI`, PARTLABEL
  `boot`), unmounted at runtime. To inspect or change kernels / the
  bootloader, mount it yourself to a temp dir and unmount when done:

      mkdir -p /mnt/boot && mount /dev/vda1 /mnt/boot
      # kernels:  /mnt/boot/simplek8s/
      # bootloader: /mnt/boot/syslinux.conf
      umount /mnt/boot

  (This mirrors what the controller does during staging.)
- **`scp` fails on SimpleK8s** — its SSH SFTP needs the legacy protocol:
  use `scp -O`, or push a script via
  `ssh host 'sh -s arg1 arg2' < script.sh`.
- Disk layout: `vda1` vfat boot (LABEL `EFI`); `vda2` ext4 (LABEL `var`)
  mounted at `/var` (the OS lives under `/var`).

## Key references

- Per-node boot-intent annotation: `simplek8s.org/next-kernel` (the release
  `ts`, e.g. `202609061935`).
- Update ConfigMap: `simplek8s/simplek8s-controller` — keys
  `updates.update-mode` (`off`|`stage`|`full`), `updates.check-interval`,
  `updates.url`, `updates.preserve`, `updates.max-percent-usage`.
- Release repo: `https://dl.simplek8s.org/simplek8s/dev/`.
- Reboot API: `POST/GET/DELETE /api/v1/reboots[/{node}]` (see
  `internal/api/api.go`).
