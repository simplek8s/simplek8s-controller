// Package updatecore is the k8s-agnostic update core (PLAN-M6 reuse
// boundary): index fetch/verify, download, extract, staging, purge,
// bootloader writers, boot-device discovery + mount, version/flavor
// helpers and the local-node CLI surface (localcli.go).
//
// Invariant: this package MUST NOT import internal/kube,
// internal/engine (or any cluster-coupled package) — neither
// directly nor transitively. cmd/nodectl links only this
// package (+ stdlib and the module's two base deps), so the node
// binary stays cluster-free by construction. The cluster wiring
// (Feature, Check against *kube.Node, enqueue, reconcile, migrate)
// lives in internal/features/update and imports this package.
// Enforced by `make cli-no-kube`.
package updatecore
