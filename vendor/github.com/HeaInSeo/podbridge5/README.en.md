# podbridge5

`podbridge5` is a Go library for container-based execution environments.
Its purpose is to provide one codebase for image builds, image pushes, container lifecycle operations, and volume data injection.

This project is not Kubernetes-specific.
It can sit under a Kubernetes-oriented system, but `podbridge5` itself is being shaped as a more general container runtime utility layer.

## Core responsibilities

- create, start, and inspect containers
- manage the healthcheck contract for runtime execution
- assist with image build and push flows
- create, replace, and reuse named volumes
- copy data between host directories and volumes
- provide a reproducible runtime test path

## Typical use cases

- backends for container-based tools
- runtime helpers in build and delivery pipelines
- development-time container sandboxes
- runtime adapters below orchestration layers
- container automation outside Kubernetes

## Project direction

Two things matter most in the current codebase.

1. separating runtime-dependent code from pure logic
2. keeping runtime validation reproducible on a clean VM

When something fails, it should be easier to tell whether the problem comes from
- pure logic
- or actual Podman/Buildah/storage initialization paths

## Go baseline

This repository uses `Go 1.25.6` as declared in `go.mod`.
The policy is to stay aligned with the same `Go 1.25.x` baseline used by sibling projects.

- The main entry points are `make test-unit`, `make test-runtime`, and `make test-runtime-integration`.
- Those targets validate that the active `go` binary is `go1.25.6` before running tests.
- The goal is not backward compatibility with older Go releases. The environment, CI, and VM path should match `1.25.x`.

## Kubernetes user namespace build path

For NodeVault's rootless builder migration, `podbridge5` now exposes Buildah defaults for Kubernetes user namespace Pods.
`DefaultUserNamespaceStoreOptions()` builds the equivalent of the required `storage.conf` in code: `/storage` based `runroot`, `graphroot`, overlay storage, and partial image pulls.
Use `WithStoreRoots`, `WithStoreDriver`, `WithFuseOverlayfsMountProgram`, and `WithPartialImagePulls` when a cluster needs different storage behavior.

Build options are available through `UserNamespaceImageBuildOptions()` and `BuildDockerfileContentUserNamespace()`.
Start new build-and-push calls with `NewUserNamespaceBuildConfig()` to make OCI isolation and the storage mode explicit. For compatibility, an empty `Isolation` in the legacy `UserNamespaceBuildConfig{}` still resolves to `chroot`; public consumers should set isolation explicitly. Harbor cache refs can be wired to both cache-from and cache-to through `CacheRef`.
`DefaultUserNamespaceBuildEnvironment()` and `DefaultUserNamespaceBuildCapabilities()` are examples, not deployment requirements. Validate capabilities, AppArmor, and the storage driver for the target runtime; the library does not guarantee a Kubernetes security policy.

### Persistent Multipass runtime VM

The Buildah runtime-validation VM, `podbridge5-dev`, is managed by the HCL in [infra/multipass](infra/multipass/README.md). It is a pd5-specific test host and intentionally separate from the `infra-lab` Kubernetes infrastructure.

```bash
cd infra/multipass
tofu init && tofu apply

cd ../..
make vm-prepare-runtime REMOTE_USER=seoy
make vm-sync-runtime REMOTE_USER=seoy
make vm-run-runtime REMOTE_USER=seoy
```

`make vm-test-runtime` is a compatibility path that deletes the VM after testing. Use the prepare/sync/run sequence above for the HCL-managed persistent VM.
Overlay remains the target default, but NodeVault migration still needs separate validation for node filesystem/idmap behavior or the fuse-overlayfs path.

This path assumes Kubernetes 1.36 user namespaces, a Linux kernel/filesystem combination with idmapped mount support, containerd 2.x, and crun 1.9+.
Incompatible Dockerfiles should later be routed by NodeVault to an isolated privileged fallback strategy.

## Changelog

- `v0.1.8` — Added public `SaveImage`/`ImageArchivePath` (thin exported wrappers around `saveImage`/`imageArchivePath`). Lets a caller export a built image to a tar/tar.gz archive and inspect its file list without executing anything inside it (issue #2, needed for NodeVault's final-image risky-tool scan)
- `v0.1.7` — lint fixes (importShadow, nil-context patterns); added unit tests for `NewConfigFromFile`, `GenerateExecutor`, and `ProcessScript`. Deleted `RunContainerWithStats` (was marked for removal, zero callers). Changed `BuildConfig.CreateImage()` signature to `(ctx context.Context, store storage.Store)`, eliminating the global-state (`pbCtx`/`pbStore`) dependency and aligning with `CreateImageWithDockerfile`. Cleaned up TODO comments; improved GitHub Actions VM runtime-test workflow SSH secret handling
- `v0.1.6` — added the `NewUserNamespaceBuildConfig()` constructor and split out `ClassifyBuildahExecutionError()`; added a `NetworkConfiguration` option on `UserNamespaceBuildConfig`. Fixed a bug found via remote VM validation: rootless netavark couldn't set up a network namespace during `RUN` steps under OCI isolation (`setns: Operation not permitted`); worked around with `NetworkConfiguration: NetworkDisabled`. Added an OpenTofu module under `infra/multipass` to manage the persistent validation VM (`podbridge5-dev`)
- `v0.1.5` — expanded runtime/integration test coverage (store, builder, registry push/pull, user-namespace build, rootless init path). Bugs found and fixed: removed hardcoded `runc` (VM only has `crun`), fixed build-layer leak from missing `RemoveIntermediateCtrs`, removed unnecessary netavark network setup, corrected `Chmod` default (`"0o755"` → `"755"`), adjusted volume-cleanup timeout. Replaced `unshare -r` with `podman unshare` in the remote VM test harness to apply the full subuid/subgid range and export rootless env vars correctly. Combined test coverage reached **80.5%**
- `v0.1.4` — removed fuse-overlayfs fallback from `MountOverlay` (native rootless overlay only), clarified `CreateContainer` create-if-absent contract and cleaned up related tests, fixed remote VM runtime test pipeline (socket permissions, output-stream stalls). Combined test coverage reached **70%+**
- `v0.1.3` — fixed golangci-lint wrapcheck/govet/gocritic findings; added changelog section to README
- `v0.1.2` — updated Podman/Buildah dependencies, fixed remote VM user namespace integration, added user namespace Buildah defaults
- `v0.1.1` — replaced `seoyhaein/utils` with `HeaInSeo/utils v0.0.7` (no API changes)

## Documentation

- Korean runtime validation: [docs/runtime-testing.ko.md](docs/runtime-testing.ko.md)
- English runtime validation: [docs/runtime-testing.en.md](docs/runtime-testing.en.md)
- current refactor sprint: [docs/sprint-2026-04-14-runtime-refactor.md](docs/sprint-2026-04-14-runtime-refactor.md)
- legacy README backup: [backup/Readme.legacy.md](backup/Readme.legacy.md)

## Current validation model

Fast checks happen locally, while runtime-dependent validation runs on a remote Multipass VM.
The detailed flow and Makefile targets live in the runtime validation documents.
