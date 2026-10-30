# SimpleK8s Controller & nodectl

![SimpleK8s Controller logo](assets/icon.png)

SimpleK8s is a **simple** Kubernetes distro: easy to install on a node,
easy to keep updated as a cluster. This repo holds its two management
components:

| Component | Where it runs | What it does | Details |
| --- | --- | --- | --- |
| **nodectl** | on each node, no cluster needed | install the distro to disk, stage kernel releases, check/update the boot entry, web setup wizard, self-updates itself | [docs/nodectl.md](docs/nodectl.md) |
| **simplek8s-controller** | in the cluster, as a privileged DaemonSet | stages signed releases on every node's boot partition and reboots nodes safely (PDB-aware, concurrency-limited, window-gated) | [docs/controller.md](docs/controller.md) |

Design doc: [PLAN.md](PLAN.md).

## nodectl in 1 minute

On the node itself, as root (`nodectl` manages only the local node):

```sh
nodectl check                    # newest release vs running vs staged
sudo nodectl update              # stage newest verified release + re-point boot
sudo nodectl install /dev/vda    # install the distro onto another disk (live-booted)
sudo nodectl wizard              # web setup UI: https://<node-ip>:5443
```

Full command reference: [docs/nodectl.md](docs/nodectl.md).

## controller in 2 minutes

On a machine with cluster-admin `kubectl` + `helm`, against
SimpleK8s-distro nodes:

```sh
# install (stages updates by default, never auto-reboots)
helm -n simplek8s install simplek8s-controller \
  oci://ghcr.io/simplek8s/charts/simplek8s-controller --create-namespace

# automatic reboots: open a window first (canary first, always)
helm -n simplek8s upgrade simplek8s-controller \
  oci://ghcr.io/simplek8s/charts/simplek8s-controller --reuse-values \
  --set 'config.reboots.windows=["@daily"]'

# schedule a reboot (no Service by design, via port-forward)
kubectl port-forward -n simplek8s daemonset/simplek8s-controller 1880:8080 &
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"nodes":["*"],"requestedBy":"user","force":false}' \
  http://127.0.0.1:1880/api/v1/reboots
```

What it does and how to run it: [docs/controller.md](docs/controller.md).

## Development

```sh
make                # vet + test + build (both binaries, amd64+arm64)
make build-nodectl  # static nodectl binaries (amd64+arm64) under build/
make image          # controller docker image
make deploy         # helm upgrade --install ./chart (uses your kubeconfig)
```

## License

Licensed under the Apache License, Version 2.0, see [LICENSE](LICENSE).
