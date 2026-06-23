#!/usr/bin/env bash
# Re-applies vendor/ patches that `go mod vendor` would otherwise undo.
# Run automatically by `make vendor` after `go mod vendor`.
set -euo pipefail
cd "$(dirname "$0")/.."

# btrfs.go/version.go require <btrfs/version.h> (libbtrfs-devel), which Rocky/RHEL
# does not package. NodeVault always builds with exclude_graphdriver_btrfs, so the
# registration is removed outright — this also makes bare `go test ./...` work
# without BUILDTAGS, since nothing pulls in the btrfs package anymore.
cat > vendor/go.podman.io/storage/drivers/register/register_btrfs.go <<'EOF'
// btrfs graphdriver registration removed by scripts/patch-vendor.sh: btrfs.go/version.go
// require <btrfs/version.h> (libbtrfs-devel), unpackaged on Rocky/RHEL hosts, and
// NodeVault always builds with exclude_graphdriver_btrfs (see Makefile BUILDTAGS).
package register
EOF
