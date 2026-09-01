# simplek8s-controller

A node-management controller for self-built Kubernetes clusters
(custom Buildroot-based distro, kubeadm). It runs as a privileged
DaemonSet with one instance per node and provides operational
capabilities over the hosts.

First capability (v1): **scheduled node reboots** with a concurrency
limit, availability waiting, and PodDisruptionBudget awareness.

- Project: https://simplek8s.org
- Repo: https://github.com/simplek8s/simplek8s-controller
- Go 1.27, stdlib only (no client-go).

## Status

In **PLAN** phase — see [PLAN.md](PLAN.md).

## Layout (planned)

```
cmd/simplek8s-controller/   binary
internal/                   kube client, engine, features (reboot), api
deploy/                     kustomize (SA, RBAC, DaemonSet)
```
