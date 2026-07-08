# NodeKit/NodeVault Live Recipe Extended Test - 2026-07-08

## Scope

This is the second live test pass for the NodeKit CLI to NodeVault Kubernetes data-plane flow.
The first report remains in `docs/NODEKIT_LIVE_RECIPE_REPRO_TEST_2026-07-08.md`.

Environment:

- NodeVault repo: `main` at `327b8ba`
- NodeKit repo: `main` at `9ca1f06`
- Kubernetes env: `seoy-libvirt-cilium`
- Backend: libvirt
- CNI: cilium
- Nodes: 3 Ready
- NodeVault pod: `nodevault-controlplane-5b795bf9f8-gvqvg`
- Exposure used for CLI tests: ClusterIP service plus `kubectl port-forward`
- NodePort: not used

Run directory during execution: `/tmp/nodekit-ext-20260708T050507Z`.
The durable record is this document; temporary scripts and transcripts are not part of the required test method.

## Result Summary

| ID | Test | Result | Notes |
| --- | --- | --- | --- |
| 1 | ResolveRecipe candidate selection | PASS | Re-run with port-forward open: `GrpcResolveRecipeClientIntegrationTests.ResolveAsync_KnownCondaPackage_ReturnsCandidates` passed. Initial harness run failed because the port-forward had already closed. |
| 2 | Harbor spec referrer/integrity | FAIL | Successful builds still lacked `spec_referrer_digest`; NodeVault logged `spec referrer push failed (integrity_health=Partial)` with Harbor CA verification failure. |
| 3 | Reconcile normal path | FAIL | Reconcile repeatedly used `http://harbor.lab.local:80` and timed out while checking known pushed images. |
| 4 | CLI digest/push event display | FAIL | `nodekit build submit` reached `Succeeded`, but stdout did not expose the pushed image digest or a clear push-digest event. |
| 5 | Failed-build contamination | PASS | Failed recipe stable refs from the first pass were not present as `Active` entries in NodeVault's index. |
| 6 | Cancel behavior | PASS | Ctrl-C returned exit code 130, printed cancellation, and did not create an `Active` registry entry for the cancelled recipe. |
| 7 | Same stableRef rebuild | PASS | Two successful builds with the same stableRef created two pinned entries, so history was retained. |
| 8 | Harbor manifest API check | PARTIAL | In-cluster Harbor endpoint was reachable over HTTPS, but unauthenticated `HEAD` returned `401 Unauthorized`; this confirms reachability but not manifest existence through an authenticated client. |
| 9 | Interactive create smoke | PASS | Initial harness command was invalid (`--accept-dockerfile-warning` without dockerfile mode). Correct quick-setup transcript exited 0 and wrote a recipe. |
| 10 | SourceBuild dependency handling | FAIL | `BuildDependencies` is not rendered or validated by the recipe renderer path, so SourceBuild still relies on the base image already containing tools such as `curl`, `sha256sum`, and `tar`. |

## Confirmed Failures

### F-01: Spec referrer push is still failing

NodeVault successfully built and registered images, but spec referrer publication remained partial.

Observed log pattern:

```text
spec referrer push failed (integrity_health=Partial)
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Impact:

- Image push can succeed while reproducibility metadata publication fails.
- `spec_referrer_digest` remains absent for successful builds.
- Reproducibility health is not fully satisfied even when the build itself is `Succeeded`.

### F-02: Reconcile uses the wrong Harbor scheme/port

NodeVault reconcile repeatedly checked Harbor with plain HTTP:

```text
HEAD http://harbor.lab.local/v2/library/.../manifests/sha256:...
dial tcp 10.113.24.96:80: i/o timeout
```

Impact:

- Reconcile cannot verify images that were pushed to the HTTPS Harbor endpoint.
- Registry state can remain stale or repeatedly fail health checks.

### F-03: CLI success output hides the final pushed digest

For a successful build submit, NodeKit printed:

```text
[로그] spec 해결 완료 (digest: a073650a411f0303...)
[빌드 시작] 빌드 제출됨 (build ID: 719aea52-459e-4710-af91-6032845f1ec3)
[로그] build state: Building
[로그] build state: Pushing
[성공] build state: Succeeded
```

It did not print the final image digest or a clear `PUSH_SUCCEEDED`/`DIGEST_ACQUIRED` event.

Impact:

- A user cannot verify the exact pushed artifact from the CLI success transcript alone.
- The reproducibility path depends on reading NodeVault registry/index state separately.

### F-04: SourceBuild dependencies are not actionable

The earlier SourceBuild failures showed that bases without `curl` fail at build time. The extended check confirmed that `BuildDependencies` is not rendered or validated in the recipe renderer path.

Impact:

- SourceBuild recipes are only reliable when the selected base image already contains required fetch/extract/checksum tools.
- The recipe model can imply dependencies that the builder does not install.

## Partial / Inconclusive Items

### Harbor manifest API

An in-cluster HTTPS `HEAD` to Harbor returned:

```text
HTTP/1.1 401 Unauthorized
www-authenticate: Bearer realm="https://harbor.lab.local/service/token",service="harbor-registry",scope="repository:library/nodekit-ext-20260708t050507z-digest-event:pull"
```

This confirms network reachability and Harbor auth challenge behavior, but the test did not use credentials. Treat this as a partial check, not proof that the manifest is missing.

## Positive Controls

- Cluster was registered in infra-lab and reachable through MCP.
- Kubernetes data-plane app was running and Ready.
- No NodePort exposure was used.
- NodeKit ResolveRecipe live client test passed when port-forward was active.
- Cancelled and failed builds did not pollute active tool registry entries.
- Same stableRef rebuild retained multiple pinned entries.
- Interactive quick-setup recipe creation works with a valid transcript.

## Next Fix Order

1. Fix Harbor trust/scheme handling for referrer push and reconcile together.
2. Add CLI output for final pushed image digest and push event visibility.
3. Make SourceBuild dependency behavior explicit: either install/render `BuildDependencies` or reject recipes whose base image cannot satisfy required tools.
4. Add an authenticated Harbor manifest/referrer check after Harbor credentials are wired into the test harness.
