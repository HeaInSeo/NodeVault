# NodeVault Reproducibility Improvement Plan

Date: 2026-07-08
Scope: NodeVault build/referrer/reconcile/event metadata and reproducibility state
Related:

- `docs/NODEKIT_LIVE_RECIPE_REPRO_TEST_2026-07-08.md`
- `docs/NODEKIT_LIVE_RECIPE_EXTENDED_TEST_2026-07-08.md`

## Background

The live NodeKit to NodeVault tests confirmed that the core Kubernetes data-plane path works:

```text
NodeKit recipe submit
-> NodeVault ResolveToolSpec / SubmitToolBuild
-> in-Pod Buildah build
-> Harbor image push
-> image_digest persisted
-> NodeVault index lifecycle_phase=Active
```

The remaining NodeVault-side issues are not basic build failures. They are reproducibility metadata, registry trust, reconcile, and downstream artifact contract problems.

Confirmed issues:

```text
1. ORAS ToolSpec referrer push fails against Harbor because ORAS does not share the Buildah CA trust path.
2. Reconcile checks Harbor through plain HTTP and times out.
3. Build/watch output does not carry the final image digest, referrer digest, or integrity status.
4. lifecycle_phase and integrity_health need explicit separate semantics.
5. SourceBuild final image policy needs enforcement because ToolSpec images become ToolFunctionSpec base images.
```

## Design Position

NodeVault should treat a successful image build as only one part of reproducibility.

```text
lifecycle_phase
= whether the image is usable as a registered ToolSpec image

integrity_health
= whether reproducibility metadata is complete and verified
```

Example:

```text
image build/push succeeded
image_digest recorded
registry entry is Active
ToolSpec referrer push failed

=> lifecycle_phase=Active
=> integrity_health=Partial
```

This distinction is important for NodeSentinel and NodePalette. They need to know both whether an image can be used and whether its reproducibility metadata is complete.

## P0: RegistryConfig Unification

NodeVault should use one registry configuration path for:

```text
Buildah image push
ORAS spec referrer push
reconcile manifest check
digest resolve
future referrer verification
```

Proposed config:

```text
NODEVAULT_REGISTRY_ADDR=harbor.lab.local
NODEVAULT_REGISTRY_SCHEME=https
NODEVAULT_REGISTRY_CA_FILE=/etc/containers/certs.d/harbor.lab.local/ca.crt
NODEVAULT_ORAS_CA_FILE=/etc/containers/certs.d/harbor.lab.local/ca.crt
REGISTRY_AUTH_FILE=/run/containers/0/auth.json
```

Implementation notes:

- If `NODEVAULT_ORAS_CA_FILE` is unset, NodeVault should discover `/etc/containers/certs.d/<registry-host>/ca.crt`.
- Reconcile must not hardcode `http://`.
- HTTP 401 from Harbor over HTTPS should be classified as an auth challenge, not as network failure.
- Timeout, TLS failure, auth challenge, and not-found should be distinct reconcile outcomes.

Acceptance criteria:

```text
AC-REG-01: Buildah push and ORAS referrer push use the same Harbor CA trust path.
AC-REG-02: Reconcile uses RegistryConfig scheme/CA/auth instead of HTTP hardcoding.
AC-REG-03: Successful builds record spec_referrer_digest.
AC-REG-04: Referrer push failure keeps lifecycle_phase=Active only if image_digest exists, but sets integrity_health=Partial.
```

## P1: Build Events and Artifact Metadata

NodeVault should persist structured build events, not just coarse state transitions.

Required durable metadata:

```text
build_id
stable_ref
recipe_method
tool_spec_digest
image_ref
image_digest
spec_referrer_digest
integrity_health
lifecycle_phase
created_at
```

Recommended `build_events` fields:

```text
id
build_id
kind
message
image_ref
image_digest
spec_referrer_digest
integrity_health
created_at
```

Event kinds:

```text
BUILD_SUBMITTED
BUILDING
PUSHING
PUSH_SUCCEEDED
DIGEST_ACQUIRED
SPEC_REFERRER_PUSHED
SPEC_REFERRER_PARTIAL
SUCCEEDED
FAILED
CANCELLED
```

Short-term bridge:

```text
Add image_ref, image_digest, integrity_health, and spec_referrer_digest to build_state/watch responses.
Emit a synthetic DIGEST_ACQUIRED event at Succeeded time if a full event log is not ready yet.
```

Acceptance criteria:

```text
AC-EVT-01: Successful builds persist PUSH_SUCCEEDED and DIGEST_ACQUIRED.
AC-EVT-02: WatchToolBuild exposes image_digest to NodeKit.
AC-EVT-03: NodeSentinel can start scan/dry-run from NodeVault image_ref/image_digest without scraping logs.
```

## P2: SourceBuild Final Image Policy

NodeVault should enforce that ToolSpec final images are clean because ToolFunctionSpec images inherit from them.

Risky runtime tools:

```text
curl
wget
git
ssh
scp
apt
apt-get
apk
yum
dnf
mamba
conda
micromamba
gcc
g++
clang
make
cmake
```

Policy:

```text
Build stages may contain fetch/build tools.
Final ToolSpec images should not contain fetch/build tools by default.
Exceptions require explicit allowRuntimeTools and a reason.
```

Example exception:

```json
{
  "allowRuntimeTools": ["curl"],
  "allowRuntimeToolsReason": "The tool requires runtime remote reference retrieval."
}
```

Acceptance criteria:

```text
AC-SB-01: SourceBuild can produce a clean runtime final image without curl.
AC-SB-02: curlimages/curl is allowed as a fetch/build stage but rejected or warned as a final base.
AC-SB-03: BuildDependencies do not remain in the final ToolSpec image unless explicitly allowed.
AC-SB-04: Final image scan reports risky runtime tools as WARN or FAIL.
```

## P3: Pinning and Reproducibility State

NodeVault should not mark version-only package pins as complete reproducibility.

Suggested fields:

```text
pinning_status=FullPin | VersionOnly | UserProvidedManualPin | Unresolved
reproducibility_status=Complete | Partial | Weak | Rejected
```

Policy:

```text
name=version=build
=> FullPin / Complete candidate

name=version
=> VersionOnly / Weak or Rejected, depending on policy
```

Acceptance criteria:

```text
AC-PIN-01: Full pins can be represented separately from version-only pins.
AC-PIN-02: Version-only submissions are never shown as Complete reproducibility.
AC-PIN-03: NodeKit loose mode, if allowed, maps to Weak or Partial status in NodeVault.
```

## Recommended Implementation Order

1. Implement RegistryConfig and fix ORAS/reconcile trust and scheme handling.
2. Add build artifact metadata to build state/watch responses.
3. Add durable build_events.
4. Add final image risky tool scan and SourceBuild final image policy.
5. Add pinning_status and reproducibility_status fields.

## Engineering Opinion

P0 should be fixed before SourceBuild work. The tests already prove images can build and push, but NodeVault cannot yet prove the complete artifact chain because referrer and reconcile use inconsistent registry assumptions.

The second priority is the event/artifact contract. NodeKit, NodeSentinel, and NodePalette should not infer digests by parsing logs or reading private index state. NodeVault should publish the artifact identity directly.

SourceBuild multi-stage rendering is important, but it spans both repositories. NodeKit should define the recipe/UX contract first; NodeVault should then enforce the clean final image and registry metadata policy.
