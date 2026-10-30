# SimpleK8s nodectl

`nodectl` is the **on-the-node** tool of SimpleK8s. It manages **only the
local node**: no controller, no cluster access, no node annotations.
One static binary, flat subcommands, stdlib only.

- Must run **as root on the node itself** (except `version`).
- Exits: `0` ok, `1` operational error, `2` misuse,
  `3` idle (`wizard` on a worker node — nothing to manage, not an error).

## Install / update the binary

The distro ships the published artifact at `/usr/local/bin/nodectl`.
`nodectl` keeps itself current:

```sh
nodectl selfupdate                     # channel stable, embedded keyring
nodectl selfupdate --dry-run           # report only, touch nothing
NODECTL_NO_SELFUPDATE=1 nodectl check  # opt out of the daily auto-check
```

- Every invocation except `version`/`help` and `selfupdate` itself does a
  best-effort auto self-check when running as root: at most one attempt
  per 24h (state in `/run/simplek8s/nodectl-selfcheck`, tmpfs — "first
  command after each boot + 24h throttle"). Silent on success, a stderr
  warning on failure; never changes the subcommand's exit code or stdout.
  `--dry-run` anywhere suppresses it.
- A locally built binary carries a stamped channel release TS
  (`-X main.releaseTS=$(TS)`, shown by `version`); when the local stamp
  is newer than the channel's `latest`, selfupdate skips instead of
  downgrading. Unstamped builds (`go build` by hand) use the
  checksum-only rule.
- Channels for `--url`: `dev|rolling|stable` or a full base URL
  (default `https://dl.simplek8s.org/simplek8s-nodectl/stable`).
  Same keyring discipline everywhere: embedded default, `--keyring`
  override, `/dev/null` break-glass (loud warning).

Build it yourself:

```sh
make build-nodectl  # static amd64+arm64 under build/, TS-stamped
```

## Kernel releases: check / update / list / purge / boot

All of these auto-detect everything (flavor from staged names →
device-tree → build arch; boot device enumerate-then-verify; bootloader
by config presence). `--url` is the release channel (`dev|rolling|stable`
or full URL, default `https://dl.simplek8s.org/simplek8s/stable`).
Verified downloads only (GPG-signed index + sha256 per file).
`nodectl` and the controller serialize through the shared
`/run/simplek8s/update.lock` (reads shared, writes exclusive).

```sh
nodectl check [--url ...] [--verbose]
  # flavor / running / staged / remote-newest + verdict.
  # --verbose lists all remote ts of the flavor + keyring/URL.

nodectl update [<ts>] [--url ...] [--next-kernel=true] \
  [--preserve 3] [--max-percent-usage 75] [--dry-run]
  # newest release by default. Check + verified download + stage +
  # retention + re-point the bootloader default (skip with
  # --next-kernel=false). A staged file matching the index hash skips
  # the download. --dry-run prints the plan, touches nothing.

nodectl list
  # staged versions + running + bootloader default (read-only).

nodectl purge [--preserve 3] [--max-percent-usage 75] [--dry-run]
  # retention + bootloader prune. Never prompts; never deletes the
  # running version; only runs under space pressure semantics.

nodectl boot [<ts>] [--dry-run]
  # no args: show the bootloader default. With <ts>: re-point it at a
  # staged ts (absent files refused).
```

## Install the distro onto a disk

Runs on a **live-booted** SimpleK8s (ISO/USB/IMG) and provisions an
**other** disk from the release `.IMG` (`.img.zst`, streamed + verified
while writing — no `curl`/`dd` needed):

```sh
nodectl install [<ts>] <device> [--url ...] [--yes] [--dry-run] \
  [--config FILE] [--password-file FILE] [--ssh-key KEY]... [--ssh-keys-file FILE]
  # e.g. nodectl install /dev/vda
```

- Arch fully auto (device-tree model → `rpi4`/`rpi5`, else build arch).
  No `<ts>` → latest IMG; `<ts>` → that version.
- Writes the image, appends a `/var` partition (all remaining space,
  `ext4 -L var`), and writes `simplek8s/simplek8s.yaml` (mounts-only by
  default, or `--config FILE` verbatim — then nothing is ever prompted).
- Without `--config`, on a tty, prompts for the root password (hidden,
  twice, empty refused) **before** the countdown and hashes it in-process.
  Non-tty without `--config` is exit 2 (supply `--config`).
- Safety: whole-disk validation (partitions rejected, the running boot
  disk always refused), refuse when anything on the target is mounted,
  then a 10s cancellable countdown (`Enter`/Ctrl-C aborts untouched;
  non-tty requires `--yes`). Target floor: 1 GiB.
- `--dry-run` resolves the IMG and prints the plan — downloads and
  touches nothing. No update lock (the target is always another disk).

## Setup wizard (web UI)

```sh
sudo nodectl wizard [-listen 0.0.0.0:5443]
```

- Serves an embedded SPA over **HTTPS only** (ephemeral EC P-256
  self-signed cert in memory — the browser warning is expected) on all
  interfaces. No plaintext listener.
- Auth is root-only: `POST /api/login {username: "root", password}` is
  verified against the distro password (anything but `root` yields the
  same generic 401). Session cookie (`Secure; HttpOnly; SameSite=Strict`,
  30 min idle expiry) + per-IP backoff + `Origin` check on mutating
  endpoints.
- What it serves depends on node state (ground truth per request,
  server-gated — the client is never authoritative):

  | State | Meaning | Serves |
  | --- | --- | --- |
  | `live` | no `/run/simplek8s/simplek8s.yaml` (install pending) | installer only |
  | `fresh` | yaml present, no `/etc/kubernetes` | cluster `init` + `join` |
  | `worker` | joined worker (has `/etc/kubernetes`, no `admin.conf`) | nothing — exits `3` |
  | `cp` | control plane (`admin.conf` present) | token create (+list/revoke) |

- Announces itself on the machine console via
  `/run/issue.d/50-wizard.issue` (`Wizard: https://\4:5443`), removed on
  shutdown. The daily auto selfupdate pre-check runs once at startup (a
  newer binary `exec`s over the daemon, dropping active connections).
- Distro wiring: same port and `simplek8s-wizard.service` unit name as the
  published docs (`systemctl disable --now simplek8s-wizard` stays valid).

## Version

```sh
nodectl version   # embedded version/commit/build-time (+release TS when stamped). No root needed.
```
