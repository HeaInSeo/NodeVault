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

This repository uses `Go 1.25.5` as declared in `go.mod`.
The policy is to stay aligned with the same `Go 1.25.x` baseline used by sibling projects.

- The main entry points are `make test-unit`, `make test-runtime`, and `make test-runtime-integration`.
- Those targets validate that the active `go` binary is `go1.25.5` before running tests.
- The goal is not backward compatibility with older Go releases. The environment, CI, and VM path should match `1.25.x`.

## Documentation

- Korean runtime validation: [docs/runtime-testing.ko.md](docs/runtime-testing.ko.md)
- English runtime validation: [docs/runtime-testing.en.md](docs/runtime-testing.en.md)
- current refactor sprint: [docs/sprint-2026-04-14-runtime-refactor.md](docs/sprint-2026-04-14-runtime-refactor.md)
- legacy README backup: [backup/Readme.legacy.md](backup/Readme.legacy.md)

## Current validation model

Fast checks happen locally, while runtime-dependent validation runs on a remote Multipass VM.
The detailed flow and Makefile targets live in the runtime validation documents.
