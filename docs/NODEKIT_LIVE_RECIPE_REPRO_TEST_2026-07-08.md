# NodeKit -> NodeVault Live Recipe Reproducibility Test

Date: 2026-07-08

Environment:

- NodeVault: GitHub `main` at `327b8ba`, deployed to infra-lab K8s
- NodeKit: GitHub `main` at `9ca1f06`
- infra-lab env: `seoy-libvirt-cilium`
- NodeVault access: ClusterIP Service + `kubectl port-forward`
- NodePort: not used
- NodeVault pod: `nodevault-controlplane-5b795bf9f8-gvqvg`, Ready, 0 restarts
- NodeVault runtime log: `runtime_mode=incluster`, `build_backend=in-pod-buildah`

## Goal

Verify that NodeKit CLI can create multiple recipe variants, submit them to
NodeVault, and have NodeVault build images in the K8s data-plane Pod, push them
to Harbor, acquire image digests, and record reproducibility metadata.

Reproducibility gates checked:

- Base image pinned by `@sha256:<64 hex>`
- `latest` rejected
- conda/mamba/micromamba final package pin requires `name=version=build`
- SourceBuild requires `sha256:<64 hex>` source checksum
- Successful builds record Harbor `image_digest`
- Successful builds are registered as `lifecycle_phase=Active`

## Commands

Port-forward:

```bash
kubectl --kubeconfig /opt/go/src/github.com/HeaInSeo/infra-lab/state/seoy-libvirt-cilium/kubeconfig \
  -n nodevault-system port-forward service/nodevault-controlplane 50052:50051
```

NodeKit CLI pattern:

```bash
cd /opt/dotnet/src/github.com/HeaInSeo/NodeKit

dotnet run --project src/NodeKit.Cli/NodeKit.Cli.csproj --no-build -- \
  recipe create <recipe.json> --method <method> --non-interactive ...

dotnet run --project src/NodeKit.Cli/NodeKit.Cli.csproj --no-build -- \
  submit <recipe.json> --url http://localhost:50052
```

## Positive Cases

| Case | Recipe method | Result | Evidence |
|---|---|---:|---|
| p01 | DockerfileFallback | PASS | build `74264bc8-f935-4b3c-aba8-e533d0ed121a`; index `Active`; image digest `sha256:03a08979d433fe9c9eb2296da191795dabab850cbd8d420247b67f0ba53a774d` |
| p02 | BioContainer | PASS | build `ddedbfdc-20ef-4220-8846-77369c40d299`; index `Active`; image digest `sha256:176c29b3257648f63638032d0a1586e80f41061abf713cda144dec0d377270e7` |
| p03 | SourceBuild on `alpine` | FAIL | NodeVault build failed: `/bin/sh: curl: not found` |
| p03b | SourceBuild on `miniforge3` | FAIL | NodeVault build failed: `/bin/sh: 1: curl: not found` |
| p03c | SourceBuild on `curlimages/curl` | PASS | build `876b0c2e-aa67-467f-9179-6033d047656f`; index `Active`; Harbor push succeeded |
| p04 | Conda | PASS | build `94143358-7954-4521-99a7-928570672183`; full pin `bwa=0.7.17=h84994c4_5`; index `Active`; image digest `sha256:f2e7f6337fb826a188f3cd0b607b1f03e76978100f3793518cc05f1cb3d9dcad` |
| p05 | Micromamba | PASS | build `0f766bf8-cf82-4c52-ae17-1c86f9ddd03f`; full pin `bwa=0.7.17=h84994c4_5`; index `Active`; image digest `sha256:8b4334e3a465f7553aa5c3dd7b4895bf48266a9c7bf48967847568443fd907d5` |
| p06 | PackageMirror | PASS | build `cf2bd4e2-9d3f-4fd0-96f8-1f5627a1b25c`; mirror URI `https://conda.anaconda.org/bioconda`; index `Active`; image digest `sha256:c60cb1a5dd6416ca67e37613b42504916e06492754b744b53fdc856f0267fcfc` |

## Negative Cases

| Case | Intent | Result | Evidence |
|---|---|---:|---|
| n01 | Missing image digest | PASS | NodeKit L1 rejected `ImageUri` and Dockerfile `FROM`: digest missing |
| n02 | `latest` tag | PASS | NodeKit L1 rejected `latest` in `ImageUri` and Dockerfile `FROM` |
| n03 | Conda version-only package | PASS | NodeKit accepted `bwa=0.7.17`; NodeVault final gate rejected with `package pin "bwa=0.7.17" must include name=version=build` |
| n04 | Bad source checksum | PASS | NodeKit L1 rejected `SourceChecksum=sha256:bad` |
| n05 | BioContainer tag-only image | PASS | NodeKit authoring rejected empty `ImageDigest` |

## Findings

### F-01: SourceBuild Depends on Base Image Having `curl`

`SourceBuild` renders:

```dockerfile
RUN curl -fsSL -o source.tar.gz "<SourceUri>" && \
    echo "<sha256>  source.tar.gz" | sha256sum -c - && \
    tar -xzf source.tar.gz && ...
```

Both `alpine:3.20` and `condaforge/miniforge3:24.3.0-0` failed because `curl`
was not installed. The same SourceBuild succeeded with
`docker.io/curlimages/curl:8.8.0@sha256:cbe461f2f26e573c5f4296c5f6c904011e3f1296dabf53e73b3f126d689c3463`.

Classification: NodeKit UX/renderer constraint. SourceBuild should either
document/validate required base image tools (`curl`, `sha256sum`, `tar`) or
render a dependency installation step from `BuildDependencies`.

### F-02: Spec Referrer Push Fails Due Harbor CA Trust

Every successful build pushed the image and registered the tool, but NodeVault
logged:

```text
spec referrer push failed (integrity_health=Partial): ... tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Impact: image build/push/register succeeds, but tool spec referrer is not pushed
and `integrity_health` becomes `Partial`.

Classification: NodeVault/infra Harbor CA trust for ORAS referrer path. Buildah
push works; ORAS HTTPS trust does not.

### F-03: Reconcile Uses HTTP Harbor Endpoint and Times Out

After successful registrations, reconcile logs repeatedly showed:

```text
image exists HEAD http://harbor.lab.local/v2/...: dial tcp 10.113.24.96:80: i/o timeout
```

Impact: registered entries remain `Active`, but integrity checks cannot prove
Harbor reachability and stay degraded.

Classification: NodeVault/infra registry endpoint configuration for reconcile.

### F-04: NodeKit Submit Output Does Not Show Digest Event Details

`nodekit submit` returned success and build-state transitions, but the CLI output
did not display `PUSH_SUCCEEDED` or `DIGEST_ACQUIRED` details. Image digests were
confirmed from NodeVault index instead.

Classification: NodeKit CLI observability. Not a build failure, but the live
test pass/fail still depends on NodeVault-side index inspection.

## Reproducibility Verdict

The core path works for DockerfileFallback, BioContainer, SourceBuild with a
proper base image, Conda, Micromamba, and PackageMirror:

```text
NodeKit recipe create
-> NodeKit L1 validation
-> NodeVault ResolveToolSpec / SubmitToolBuild
-> in-Pod Buildah build
-> Harbor image push
-> image digest recorded
-> index lifecycle_phase=Active
```

NodeVault final gate correctly rejects conda version-only pins even though
NodeKit L1 allows `name=version` during authoring. Digest and `latest` failures
are blocked at NodeKit L1 before submit.

The remaining blockers are not the core image build path:

- SourceBuild needs a base image/tooling policy.
- ORAS spec referrer push needs Harbor CA trust.
- Reconcile needs a reachable Harbor endpoint.
