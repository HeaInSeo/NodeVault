# Agent Notes

## Runtime validation baseline

- Do not assume `go test ./...` on the local workstation is the primary runtime signal for `podbridge5`.
- This repository depends on Podman/buildah/storage behavior, including rootless/rootful runtime details.
- The preferred runtime validation path is the remote Multipass flow on `100.123.80.48`.

## Preferred test order

1. Use `make test-unit` for fast local feedback on pure logic.
2. Use `make vm-test-runtime` for runtime-sensitive validation.
3. Use `make vm-test-runtime-integration` for `runtime + integration` coverage.

## Remote host defaults

- Host: `100.123.80.48`
- User: `seoy`
- Default VM name: `podbridge5-dev`
- Go baseline: `1.25.6`

## Authentication

- Prefer SSH key or SSH agent authentication when available.
- `hack/remotevm` supports key-based auth and password auth.
- `REMOTE_PASS` is optional when SSH key auth works.

## Local host caveats

- On the current Rocky-based local host, `go test ./...` may fail before runtime execution if system packages are missing.
- Known examples:
  - `gpgme` pkg-config metadata
  - `btrfs` development headers
- Those local package issues do not override the main guidance: runtime verification should happen on the remote VM path.

## Commands

```bash
make test-unit
make vm-test-runtime REMOTE_USER=seoy
make vm-test-runtime-integration REMOTE_USER=seoy
```
