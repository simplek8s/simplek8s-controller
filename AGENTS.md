# AGENTS — operational notes

Durable notes to avoid repeating mistakes when working with the SimpleK8s
test VMs from this repo.

## PLAN.md — how to read it

`PLAN.md` is the single living design doc (shipped behavior + current
plan). It is long; **do not read it whole** — it opens with an index of
`§number → title`, and section numbers are stable while line numbers are
not. To read one section, locate it and read from there:

    grep -n '^### 3.4' PLAN.md

Cross-references use section numbers (§4.2, §6.1) and per-era decision
numbers — grep for those. Era tags: **M1** = reboots, **M2** = updates,
**M3** = windows. Historical plans (`PLAN-M1.md`, `PLAN-M2.md`,
`PLAN-M3.md`, the `E2E*` campaign files, `PLAN.FIXME.md`) were folded
into `PLAN.md` and removed from the working tree; their full text is in
git history (`git show <commit>:PLAN-M2.md`).

## SimpleK8s VM operations

- **CRITICAL — reach the VMs by IP, NEVER by hostname.** Always target the
  node's IP directly — read it from the cluster
  (`kubectl get nodes -o wide` → `INTERNAL-IP`).
- **SimpleK8s does NOT mount `/boot`** — there is no `/boot` on a running
  node. The boot partition is PARTLABEL
  `boot`), unmounted at runtime. To inspect or change kernels / the
  bootloader, mount it yourself to a temp dir and unmount when done:

      mkdir -p /mnt/boot && mount /dev/vda1 /mnt/boot
      # kernels:  /mnt/boot/simplek8s/
      # bootloader: /mnt/boot/syslinux.conf
      umount /mnt/boot

- **`scp` fails on SimpleK8s** — its SSH SFTP needs the legacy protocol:
  use `scp -O`, or push a script via
  `ssh host 'sh -s arg1 arg2' < script.sh`.

## Building the controller image

- Build it with **`make image`** (or the repo `Dockerfile`).
- A base image (`golang:1.27`, `alpine:3.20`) being **absent from the
  local docker cache is not a blocker** — the build pulls it.

## Key references

- Per-node boot-intent annotation: `simplek8s.org/next-kernel` (the release
  `ts`, e.g. `202609061935`).
- Update ConfigMap: `simplek8s/simplek8s-controller` — keys
  `updates.update-mode` (`off`|`stage`|`full`), `updates.check-interval`,
  `updates.url`, `updates.preserve`, `updates.max-percent-usage`.
- Release repo (**PROD, not a mock**):
  `https://dl.simplek8s.org/simplek8s/dev/` (latest dev releases) and
  `https://dl.simplek8s.org/simplek8s/stable` (also PROD, same keyring,
  but lagging — does not carry the newest releases). Real signed kernels;
  there is **no mock release server** and no test keyring.
- Release keyring (**PROD**): the standard image embeds
  `keys/simplek8s-pubring.gpg` (LFS) at `/etc/simplek8s/pubring.gpg`; the
  same PROD key is on the machines. The PROD repo is signed by that key,
  so the standard image verifies it directly — no variant image and no
  custom keyring needed. The optional `simplek8s-controller-keyring`
  Secret (`pubring.gpg` → `/etc/simplek8s/custom/`) is only for a
  genuinely custom (non-PROD) repo/key.
- Reboot API: `POST/GET/DELETE /api/v1/reboots[/{node}]` (see
  `internal/api/api.go`).
