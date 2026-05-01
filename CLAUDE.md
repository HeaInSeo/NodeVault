# NodeVault — Claude Code Guidelines

## 1. Responsibility boundary (immutable)

**NodeVault owns**: BuildRequest reception (gRPC server), builder Job orchestration,
DockGuard policy bundle management (`PolicyService`: .rego → opa build → .wasm → gRPC),
internal registry integration, `RegisteredToolDefinition` creation and CAS storage,
L3 kind dry-run (`ValidateService`), L4 smoke run (`ValidateService`),
and `ToolRegistryService`.

**NodeKit owns**: authoring UX, L1 static validation, `WasmPolicyChecker` execution,
`BuildRequest` generation, `AdminToolList` display, and all admin-side UI semantics.

**image build**: NodeVault 바이너리가 podbridge5를 in-process로 직접 실행 (seoy 호스트에서).
K8s Job으로 위임하지 않는다 — podbridge5(buildah) rootless 제약으로 K8s Pod 안에서 불가.

Do not implement authoring UI or L1 validation in NodeVault.

## 2. Key term boundaries (immutable)

| Term | Meaning |
|------|---------|
| `BuildRequest` | What NodeKit sends over gRPC. Input to NodeVault. |
| `RegisteredToolDefinition` | Post-L4 confirmed object. CAS-stored by NodeVault. SHA256 hash = filename. |
| `image build (L2)` | podbridge5 in-process (seoy 호스트에서 NodeVault 프로세스가 직접 실행). K8s Job 아님. |
| `AdminToolList` | NodeKit's admin view — NodeVault does NOT own or render this. |

Do not create `ToolDefinition` objects in NodeVault — that is NodeKit's draft model.
`RegisteredToolDefinition` is the only post-registration object NodeVault produces.

## 3. Package structure

```
cmd/controlplane   — NodeVault gRPC server (seoy 호스트 바이너리)
cmd/palette        — NodePalette REST server (seoy 호스트 바이너리, 별도 프로세스)
pkg/policy         — PolicyService: .rego management, opa build, GetPolicyBundle() RPC
pkg/build          — BuildService: podbridge5 in-process 빌드(L2) + L3/L4 orchestration
pkg/registry       — Harbor digest 획득
pkg/validate       — ValidateService: K8s dry-run (L3), smoke run (L4)
pkg/catalog        — ToolRegistryService: RegisteredToolDefinition CAS storage
pkg/index          — vault-index.json 이중 축 상태 관리
pkg/oras           — OCI spec referrer push (sori wrapping)
pkg/reconcile      — Harbor 현실 대조 (FastRun / SlowRun)
pkg/catalogrest    — NodePalette REST 핸들러 (cmd/palette에서 사용)
```

Do not cross package boundaries in the wrong direction (e.g., `catalog` importing `build`).

## 4. Orchestration loop — the critical path

L2's core challenge is the orchestration loop reliability, not the builder technology choice:

```
Job 생성 → Running → 이미지 빌드 → 내부 레지스트리 push → Succeeded → digest 확보 → 후속 단계
```

**Phase 2 gate**: builder Job happy-path must succeed once end-to-end before implementing
`RegisteredToolDefinition` creation, manifest generation, dry-run, or any UI work.
Do not proceed to detail work if the loop has not closed.

## 5. kubeconfig / K8s API access

NodeVault는 **seoy 호스트 바이너리**로 실행한다 — K8s Pod 아님.
podbridge5(buildah) rootless 제약으로 K8s Pod 안에서 overlay 마운트 불가.
K8s 접근은 로컬 kubeconfig 경유: L3/L4 Job 제출 전용.

`deploy/02-rbac.yaml`은 배포되어 있음 (미래 in-cluster 전환을 위한 선행 배포).
`deploy/03-nodevault.yaml` / `deploy/04-grpcroute.yaml`은 현재 미적용.

### 테스트 환경

| 환경 | kubeconfig | 레지스트리 주소 | Makefile 타겟 |
|------|-----------|----------------|--------------|
| kind | `~/.kube/config` | `10.96.0.1:5000` | `test-integration` |
| infra-lab | `../infra-lab/kubeconfig` | `harbor.10.113.24.96.nip.io` | `test-integration-infralab` |

**infra-lab 사전 조건** (최초 1회):
```bash
make deploy-infralab    # 네임스페이스 + RBAC 배포
```
infra-lab은 `multipass` 또는 `libvirt` backend로 VM을 생성합니다. 클러스터 기동/종료는 infra-lab 저장소의 `./scripts/k8s-tool.sh up|down`(필요 시 `BACKEND=libvirt`)을 사용합니다. 자세한 절차는 `docs/INFRALAB_TESTING.md` 참조.

통합 테스트는 NodeVault를 로컬 바이너리로 실행하고(`bin/nodevault`) kubeconfig로 원격 클러스터에 접속합니다.

## 6. Decision checklist before every change

- Does it move image build logic out of NodeVault into a K8s Job? **Requires explicit architectural decision — current design is in-process podbridge5.**
- Does it add authoring UI or L1 validation logic? **Block — that is NodeKit.**
- Does it touch the gRPC proto contract? **Requires coordination with NodeKit.**
- Does it add `ToolDefinition` (NodeKit draft model) to NodeVault? **Block.**
- Does it proceed to RegisteredToolDefinition/manifest/dry-run before the orchestration loop gate passes? **Block.**

## 7. Small diffs; no unrelated refactors

Each commit must have a single, stated purpose. Do not clean up surrounding code,
add comments to unchanged lines, or refactor while fixing a bug.

## 8. Warning policy

Maintain a **zero-warning baseline** with golangci-lint (`make lint`).
Run `make lint` after every change. Do not introduce new lint warnings.
Generated `.pb.go` files are excluded from lint (see `.golangci.yml`).

## 9. Validation responsibility

| Change type | Expected validation |
|---|---|
| New gRPC RPC | Unit test for handler + integration test with NodeKit |
| BuildService change | Orchestration loop test (Job create → status → push → digest) |
| PolicyService change | .rego load + `opa build` + `GetPolicyBundle()` RPC test |
| ValidateService change | kind dry-run / smoke run with local kubeconfig |
| CAS storage change | Hash consistency test (same content → same hash) |
| Refactor | Existing tests green; add tests if coverage was absent |

## 10. Completion reporting

A task is not complete until the following are stated explicitly:

- **What changed**: files and logic affected
- **Validation run**: which tests, lint checks, or manual verifications were performed
- **Results**: pass/fail counts, lint result, any regressions
- **Remaining risks**: known unknowns, deferred items, or assumptions not verified

## 11. Hidden failure mode review

Before marking a change complete, explicitly check for:

- builder Job created but status watch not started (fire-and-forget without loop closing)
- Job Succeeded but digest extraction from registry response fails silently
- `opa build` subprocess fails without error propagation to gRPC response
- CAS file written with wrong hash (content mismatch after read-back)
- dry-run returns success on schema error due to misparse of API server response
- K8s watch connection drops mid-Job without reconnect logic

## 12. 배포 시 알려진 미결 사항 (TODO)

seoy 배포 운영 중 발생하는 warning으로, 현재는 무해하나 빌드 파이프라인 완성 전 해결 필요.

| # | 증상 | 원인 | 해결 조건 |
|---|------|------|-----------|
| D-01 | `no subuid ranges found for user "nodevault" in /etc/subuid` | seoy에서 `nodevault` 서비스 계정의 subuid/subgid 범위 미설정. podbridge5 rootless 빌드 시 user namespace UID 매핑에 필요. | seoy에서 `sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 nodevault` 실행. 실제 BuildRequest가 들어오기 전 필수. |
| D-02 | `ValidateService unavailable (kubeconfig missing?)` | `/opt/nodevault/kubeconfig` 파일 없음. L3 dry-run / L4 smoke run은 K8s 접근 필요. | seoy K8s kubeconfig를 `/opt/nodevault/kubeconfig`로 배포. `deploy-seoy.sh`에 kubeconfig 복사 단계 추가 예정. L3/L4 구현 전 필수. |

> **D-01 / D-02는 기동 자체를 막지 않는다.** gRPC Ping, PolicyService, BuildRequest 수신까지는 정상 동작.

## 14. NodeVault 전환 계획 (진행 중)

전체 전환 계획은 **`docs/NODEVAULT_TRANSITION_PLAN.md`** 참조.
새 기능 구현 전 반드시 해당 문서의 우선순위와 선행 조건을 확인할 것.

### 새 아키텍처 불변 제약

**artifact 상태 이중 축** (절대 같은 필드에 섞지 않는다)

| 축 | 변경 주체 | 용도 |
|----|-----------|------|
| `lifecycle_phase` (Pending/Active/Retracted/Deleted) | NodeVault 명시적 호출 | Catalog 노출 결정 |
| `integrity_health` (Healthy/Partial/Missing/Unreachable/Orphaned) | reconcile loop | 알람/모니터링 전용 |

- Catalog 노출 조건: `lifecycle_phase = Active`만. `integrity_health`는 노출에 영향 없음.
- reconcile은 `integrity_health`만 변경. `lifecycle_phase`를 건드리는 reconcile 코드는 즉시 차단.

**index 접근 규칙**

- 모든 index read/write는 `pkg/index`를 경유한다.
- 다른 패키지(build, validate, oras 등)가 index 저장소에 직접 접근하는 것을 금지.
- `pkg/catalog` (CAS 저장) → `pkg/index`로 전환 예정. 전환 전까지 CAS는 유지.

**패키지 로드맵**

| 패키지 | 상태 | 역할 |
|--------|------|------|
| `pkg/index` | 구현 완료 | index 단일 제어 계층 (vault-index.json, 이중 축 상태) |
| `pkg/oras` | 구현 완료 | OCI spec referrer push (sori wrapping) |
| `pkg/reconcile` | 구현 완료 | Harbor 현실 대조 fast/slow loop |
| `pkg/catalogrest` | 구현 완료 | NodePalette REST 핸들러 + Harbor webhook 수신 |
| `pkg/catalog` | 현재 사용 중 | CAS 저장 (RegisteredToolDefinition / RegisteredDataDefinition) |

### 6번 결정 체크리스트 추가 항목

- Does it write to the index without going through `pkg/index`? **Block.**
- Does it change `lifecycle_phase` from a reconcile loop? **Block.**
- Does it change `integrity_health` from a lifecycle operation? **Block.**
- Does it expose Catalog entries based on `integrity_health`? **Block.**
- Does it start TODO-09b implementation before Cilium + Harbor are stable? **Block.**
