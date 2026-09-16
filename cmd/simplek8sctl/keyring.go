package main

import (
	_ "embed"
)

// embeddedPubring is the distro signing keyring (PLAN-M6 D13): the
// GPG default for release index verification, enabling TODO 5
// (keyring leaves the distro). `--keyring <path>` overrides it,
// `--keyring /dev/null` skips verification.
//
// The bytes come from keys/simplek8s-pubring.gpg (single source of
// truth, also ADDed by the Dockerfile); `make build-simplek8sctl` copies it to
// pubring.gpg next to this file before building (go:embed cannot
// reach outside the package dir, and refuses symlinks). The copy is
// gitignored — never commit it.
//
//go:embed pubring.gpg
var embeddedPubring []byte
