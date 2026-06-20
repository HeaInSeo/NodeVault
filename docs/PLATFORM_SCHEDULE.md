# Platform Schedule

버전: 3.0
갱신: 2026-06-19
기준 문서:
- `docs/ARCHITECTURE_V01.md` (NodeVault Kubernetes In-Pod 재현 가능 이미지 빌드 아키텍처 v0.1.0)
- `docs/OBSERVED_PROFILE_SPEC.md`, `docs/SECURITY_SCAN_SPEC.md`, `docs/RUNNER_NODE_SPEC.md`

---

## 현재 상태 (2026-06-17 기준)

### 저장소

| 저장소 | 역할 | 상태 |
|--------|------|------|
| [NodeVault](https://github.com/HeaInSeo/NodeVault) | K8s data-plane canonical resolver / builder / index SoT | v0.3.0 — in-pod-buildah 완료 |
| [NodeKit](https://github.com/HeaInSeo/NodeKit) | external authoring/admin tool (L1, BuildRequest) | 운영 중 |
| [NodeSentinel](https://github.com/HeaInSeo/NodeSentinel) | K8s data-plane 검증 에이전트 | L3~L5-b 완료 |
| [NodePalette](https://github.com/HeaInSeo/NodePalette) | 인증 tool 팔레트 REST 서비스 | v1.0 완료 |
| [DockGuard](https://github.com/HeaInSeo/DockGuard) | OPA/Rego Dockerfile 정책 + .wasm | 운영 중 |

### 완료된 것

| 영역 | 항목 | 커밋 |
|------|------|------|
| NodeVault | k8s-job 빌드 백엔드 제거 | PR-1 `cee6089` |
| NodeVault | in-pod-buildah (podbridge5) 단일 경로 | PR-1 |
| NodeVault | `hostUsers: false` + buildah capabilities Pod 설정 | PR-1 |
| NodeVault | ValidationResultService (gRPC + REST) | `91837e7` |
| NodeVault | ToolCheckRecord / ToolScanRecord / CertifiedToolImageRecord (Index v3) | `91837e7` |
| NodeVault | certification.Service (check+scan → 인증 결정) | `91837e7` |
| NodeVault | sentinelclient (NodeSentinel EnqueueValidationWork) | merged |
| NodeVault | Catalog REST + DockGuard PolicyService | 이전 |
| NodeSentinel | L3 dry-run, L4 smoke-run | Sprint 2 |
| NodeSentinel | L5-a functional validation + vaultclient | Sprint 3 |
| NodeSentinel | L5-b trivy-operator scan | Sprint 3 |
| NodePalette | GET /v1/palette/tools (PromotionStatus=active 필터) | Sprint 4 |

---

## 다음 작업: v0.1.0 아키텍처 구현

아키텍처 v0.1.0은 현재 `legacy BuildRequest` 경로를 `ToolSpecRequest → ResolvedToolSpec → SubmitToolBuild` 경로로 전환하는 것이 핵심이다. 섹션 16의 마이그레이션 단계를 기준으로 한다.

### NodeKit 연동 gate (2026-06-19)

- NodeVault는 K8s data-plane app이며, `ResolveToolSpec` canonical digest/index 저장 경로를 먼저 안정화한다.
- NodeKit은 data-plane 밖의 external authoring/admin tool이다. `SubmitToolBuild` API와 그 이후 `WatchToolBuild`/`CancelToolBuild` 경로가 준비되기 전까지 production build 제출을 기존 `BuildRequest` / `BuildAndRegister`로 유지한다.
- NodeKit agent는 외부 소비자 관점에서 현재 NodeVault proto를 관찰/재빌드해도 되지만, production `ToolSpecRequest`/`ResolveToolSpec`/`SubmitToolBuild` 클라이언트 경로는 아직 열지 않는다.
- `CertifiedToolImageRecord` 키 재정렬과 `TestToolScanRecord_WithDbDigest`는 Phase 5(P3) 검증 항목이다. NodeKit의 초기 `ToolSpecRequest`/`SubmitToolBuild` API entry gate는 아니지만, 신규 경로를 production으로 전환할 때의 readiness risk로 추적한다.
- 확인 기준: `/opt/dotnet/src/github.com/HeaInSeo/NodeKit`에서 `dotnet test --project tests/NodeKit.Tests/NodeKit.Tests.csproj /p:ApiProtosRoot=/opt/go/src/github.com/HeaInSeo/NodeVault/protos --no-restore` 통과.

---

### Phase 1 — Request/Spec 경계 적용

**목표**: 외부 authoring tool이 `ToolSpecRequest`를 작성하고, K8s data-plane의 NodeVault가 `ResolvedToolSpec`과 canonical digest를 생성하는 경계를 코드로 확정한다.

**배경**
현재 `BuildRequest` (proto)는 NodeKit이 빌드 파라미터를 직접 전달한다.
v0.1.0 이후에는 NodeKit 같은 외부 authoring tool이 사용자 의도만 담은 `ToolSpecRequest`를 제출하고, NodeVault가 canonical resolve를 담당한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `protos/nodevault/v1/nodevault.proto` | `ToolSpecRequest`, `ResolveToolSpec` RPC, `ResolvedToolSpec` 메시지 추가 |
| `pkg/resolve/` (신규) | `Resolver`: pinned base image digest 추출, recipe/build plan canonical digest 계산 |
| `pkg/resolve/digest.go` | `RecipeInputsDigest`, `BuildPlanDigest`, `ToolSpecDigest` 계산 |
| `pkg/index/schema.go` | `ResolvedToolSpec` 저장 슬라이스 추가 (Index v4) |
| `pkg/index/store.go` | `UpsertResolvedToolSpec`, `GetResolvedToolSpecByDigest` |
| `cmd/controlplane/main.go` | `ResolveToolSpec` gRPC handler 등록 |
| `pkg/build/service.go` | `SubmitToolBuild(toolSpecDigest)` — Index에서 ResolvedToolSpec 참조 |
| NodeKit: `BuildRequestFactory.cs` | `ToolSpecRequest` 작성 경로 추가 (legacy adapter 병행) |

**완료 판정**

- [x] 동일 `ToolSpecRequest` + `resolveContext` → 동일 `toolSpecDigest` (결정론 테스트)
- [x] `TestRecipeInputsDigest_Deterministic` 통과
- [x] `TestBuildPlanDigest_IncludesBuilderIdentity` 통과
- [x] `TestToolSpecDigest_Stability` 통과 (기존 casHash 방식과 독립)
- [x] `TestResolveToolSpec_StoresInIndex` 통과
- [x] `TestUpsertResolvedToolSpec_Duplicate_ReturnsExisting` 통과
- [x] `TestBuildPlanDigest_IncludesBaseImageDigest` 통과
- [x] `TestResolve_ExtractsPinnedBaseImage` 통과
- [x] `TestResolve_UnpinnedBaseImageRejected` 통과
- [x] `TestResolveToolSpec_UnpinnedBaseImage_InvalidArgument` 통과
- [x] `make test` (`go test ./...` with NodeVault build tags) 전체 통과
- [ ] registry 조회가 필요한 base image tag → digest resolve는 K8s/registry 접근 가능한 환경에서 별도 구현/검증

---

### Phase 2 — Build 경로 정리

**목표**: NodeVault Build Manager → podbridge5 → Buildah 경로에서 durable build state와 cancel/timeout을 확보한다. (아키텍처 v0.1.0 §6)

**배경**
현재 `BuildAndRegister`는 단일 RPC 안에서 build/register를 처리한다.
v0.1.0 이후에는 `SubmitToolBuild`로 build를 제출하고 `WatchToolBuild`로 스트리밍한다.
또한 Pod 재시작 후 in-progress build 상태를 `Interrupted`로 정확히 복구해야 한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/buildstate/` (신규) | SQLite 기반 durable build state: `Requested → Resolving → Building → Pushing → Succeeded/Failed/Interrupted` |
| `pkg/buildstate/store.go` | WAL, atomic transition, `RecoverInterrupted()` |
| `pkg/build/submit_tool_build.go` | `SubmitToolBuild`, `WatchToolBuild`, `CancelToolBuild` background execution |
| `pkg/build/builder.go` | podbridge5 cancel 신호 전달, subprocess cleanup |
| `deploy/03-nodevault.yaml` | graphroot/runroot PVC 전환 (emptyDir → PersistentVolumeClaim) |

**완료 판정**

- [x] `TestBuildState_RecoverInterrupted` — Pod 재시작 시 Running → Interrupted
- [x] `TestBuildState_NeverAutoSucceedInterrupted` — Interrupted를 Succeeded로 오판하지 않음
- [x] `TestBuildState_RecoverInterrupted_LeavesTerminalRecords` 통과
- [x] `SubmitToolBuild`가 buildable `raw_spec`을 decode하여 background podbridge5 build를 실행
- [x] `WatchToolBuild`가 durable status 변화를 stream하고 terminal state에서 종료
- [x] `CancelToolBuild`가 active build context와 durable state를 함께 Interrupted로 전환
- [ ] `TestBuildCancel_CleansUpSubprocess` 통과
- [ ] graphroot가 PVC에 유지됨 (Pod restart 후 layer cache hit 확인)
- [ ] `go test ./...` 전체 통과

---

### Phase 3 — Rootless UserNS 전환 검증

**목표**: 대표 ToolSpec에 대해 `hostUsers: false`, `privileged: false`로 Buildah 빌드가 실제로 동작하는지 seoy(100.123.80.48) 클러스터에서 확인한다. (아키텍처 v0.1.0 §7)

현재 `deploy/03-nodevault.yaml`에 `hostUsers: false`는 적용되어 있으나 실제 Buildah storage driver / isolation mode 동작 여부는 미확인이다.

**검증 매트릭스**

| 항목 | 목표값 | 확인 방법 |
|------|--------|-----------|
| `hostUsers` | false | Pod spec 확인 |
| `privileged` | false | Pod spec 확인 |
| `allowPrivilegeEscalation` | false | Pod spec 확인 |
| storage driver | overlay 유지 (`vfs`/`fuse-overlayfs` fallback은 별도 재검증 필요) | buildah info |
| isolation mode | chroot | buildah build 로그 |
| Harbor push → imageDigest 확인 | 일치 | ToolBuildRecord 대조 |
| rootless 실패 시 자동 privileged fallback | 없음 | normalizeBuildBackend 코드 확인 |

**완료 판정**

- [ ] seoy에서 `make deploy-infralab` 성공
- [ ] 대표 Dockerfile (alpine-based) `privileged: false`로 빌드 성공
- [ ] `ToolBuildRecord.Backend == "in-pod-buildah"` 확인
- [ ] Harbor에 image push 및 imageDigest 기록 확인
- [ ] rootless 실패 시 `BuildEventKind_FAILED` 반환 (fallback 없음) 확인

---

### Phase 4 — 캐시 계층 적용

**목표**: package cache / build layer cache / base image cache를 분리해 cold build 비용을 줄인다. (아키텍처 v0.1.0 §8)

**주요 작업**

| 항목 | 내용 |
|------|------|
| package cache PVC | conda/micromamba cache를 전용 PVC로 분리 |
| Harbor build layer cache | `--cache-from/--cache-to` harbor.local/cache/tool-family |
| cache key 설계 | `package-manager + platform + arch + channel-snapshot + lock-digest` |
| cache GC | NodeVault maintenance loop — high watermark 기반 eviction |
| `ToolBuildRecord` cache 필드 | `packageCacheHit`, `layerCacheHits`, `layerCacheMisses` |

**완료 판정**

- [ ] 동일 ToolSpec 두 번째 빌드에서 `packageCacheHit: true`
- [ ] `layerCacheHits > 0` (Harbor cache hit)
- [ ] `TestCacheGC_DoesNotEvictActiveBuildLayers` 통과
- [ ] cache 용량 상한 초과 시 신규 build 거절 (readiness 분리)

---

### Phase 5 — Record와 Certification 통합 (현재 구현 검증)

**목표**: 현재 구현된 ValidationResultService / certification.Service / CertifiedToolImageRecord 흐름이 아키텍처 v0.1.0 §12와 일치하는지 확인하고, 누락 필드를 보완한다.

**확인 항목**

| 현재 구현 | v0.1.0 요구 | 상태 |
|-----------|-------------|------|
| `ToolBuildRecord.Backend` | `execution.mode` / `execution.hostUsers` 등 분리 필요 | 구현 완료 (`mode`, `host_users`, `storage_driver`, `isolation`) |
| `ToolBuildRecord.NanVersion` | v0.1.0에 nan 미정의 — 필드 용도 재검토 | 미결 |
| `CertifiedToolImageRecord` 키 | `toolSpecDigest + platform` | 구현 완료. platform 없는 과거 record는 imageDigest compatibility lookup 유지 |
| `ToolScanRecord.DbDigest` | scanner + scannerVersion + dbDigest | 구현 완료 (gRPC + REST ingestion) |
| Harbor OCI referrer | spec referrer / toolprofile referrer | toolspec 구현 완료; toolprofile push 구현 완료 (`PushToolProfileReferrer` + `ObservedProfileDigest`); retention(latest 3개, index-local GC marking) 구현 완료 (`docs/OBSERVED_PROFILE_SPEC.md` §5) |

**완료 판정**

- [x] `ToolBuildRecord`에 `execution.*` 필드 추가 (backward-compatible optional)
- [x] `CertifiedToolImageRecord` 키를 `toolSpecDigest + platform`으로 재정렬 (Phase 1 이후)
- [x] `TestToolScanRecord_WithDbDigest` 통과

---

### Phase 6 — Legacy API 축소

**목표**: `BuildRequest / BuildAndRegister`를 deprecate하고 신규 경로로 전환한다. (아키텍처 v0.1.0 §10.3)

이 단계는 NodeKit이 `ToolSpecRequest` 경로를 완전히 채용한 이후에 진행한다.

**전환 원칙**

```text
Legacy BuildRequest
  → compatibility adapter
  → ToolSpecRequest draft 생성
  → NodeVault resolve
```

**완료 판정**

- [ ] NodeKit legacy BuildRequest usage 0
- [ ] `BuildAndRegister` RPC가 deprecated 표시 상태
- [ ] legacy usage 0 확인 후 제거 ADR 작성

---

### 병렬 트랙 A — OCI referrer (spec + toolprofile)

Phase 1 이후 병행 가능.

| 항목 | 내용 |
|------|------|
| toolspec referrer | `PushToolSpecReferrer` — 구현 완료, build 등록 후 Harbor referrer + index/reconcile 연결 |
| toolprofile referrer | `PushToolProfileReferrer` — 구현 완료, NodeSentinel의 `SubmitToolCheckRecord`(succeeded + validationHash 有)가 트리거, `index.Entry.ObservedProfileDigest`에 캐시 |
| artifactType | `application/vnd.nodevault.toolprofile.v1+json` |
| retention | latest `index.DefaultToolProfileReferrerRetain`(=3)개 — 구현 완료. `pkg/index/store.go:RecordToolProfileReferrer`가 index-local로 `ACTIVE`/`GC_CANDIDATE` 마킹만 수행 (registry push/delete 없음). 물리적 삭제는 Harbor GC 정책에 위임. 조회: `GET /v1/gc/toolprofile-candidates` |

---

### 병렬 트랙 B — L5-a 실 검증 (sample fixture)

현재 L5-a는 `/bin/sh -c true` (기동 확인만). 실제 sample fixture 마운트와 observedIoProfile 수집으로 격상.

| 파일 | 변경 |
|------|------|
| `pkg/worker/l5a.go` | sample fixture ConfigMap 마운트 + output emptyDir 수집 |
| `pkg/worker/l5a.go` | `observedIoProfile`: 포트별 파일 존재·개수·크기 |
| `pkg/worker/l5a.go` | `contractCheck`: 선언 output 존재 여부 |

---

### 병렬 트랙 C — 운영 안정화

| 항목 | 내용 | 우선순위 |
|------|------|---------|
| ValidateService RBAC 이관 | L3/L4 Job 권한 NodeVault SA → NodeSentinel SA | P3 |
| Harbor 인증 Secret 통합 | buildah용 + ORAS용 분리 → 단일 정리 | P3 |
| Data write path | DataRegisterRequest gRPC 경로 완성 | P3 |
| DagEdit ↔ NodePalette 연결 | GET /v1/palette/tools → casHash pin | P4 |

---

## 전체 우선순위 요약

```
즉시 (P1)
  └── Phase 1: ToolSpecRequest / ResolvedToolSpec 경계 적용
  └── Phase 3: Rootless UserNS seoy 클러스터 검증 (병행 가능)

단기 (P2)
  ├── Phase 2: durable build state + cancel/timeout
  ├── 트랙 A: OCI referrer
  └── 트랙 B: L5-a sample fixture

중기 (P3)
  ├── Phase 4: 캐시 계층
  ├── Phase 5: Record/Certification 통합 검증
  └── 트랙 C: 운영 안정화

장기 (P4)
  └── Phase 6: Legacy API 축소
```

---

## 공통 gate (모든 Phase 적용)

- `go test ./...` 전체 통과, `make lint` 경고 없음
- 기존 `casHash` 계산 방식 변경 금지 (Phase 1에서 toolSpecDigest와 병행)
- Index entry backward compatibility 유지 — 신규 필드는 `omitempty` optional
- K8s Job / 외부 Pod 생성 로직을 NodeVault에 추가 금지
- rootless 실패 시 자동 privileged fallback 금지 (아키텍처 v0.1.0 §7.8)
- durable build state 없이 in-memory에만 build 상태 저장 금지

---

## 미결정 사항 (아키텍처 v0.1.0 §18 기준)

| # | 항목 | 결정 시점 |
|---|------|-----------|
| 1 | podbridge5 library link vs binary subprocess | Phase 2 구현 시 |
| 2 | Buildah isolation 기본값 (chroot vs rootless/OCI) | Phase 3 검증 결과 |
| 3 | graphroot/runroot 동일 PVC vs 분리 PVC | Phase 2/4 |
| 4 | builder.imageDigest exact component boundary (podbridge5 포함 여부) | Phase 1 설계 시 |
| 5 | NodeVault replica 수 + single-writer build scheduling | Phase 2 이후 |
| 6 | nan runtime binary 주입 — ToolFunctionSpec 구현 시 함께 정의 | ToolFunctionSpec 설계 단계 |

---

## 참조 문서

| 문서 | 내용 |
|------|------|
| `docs/ARCHITECTURE_V01.md` | NodeVault In-Pod 빌드 아키텍처 v0.1.0 전문 |
| `docs/PLATFORM_MAP.md` | 전체 컴포넌트 지도 |
| `docs/OBSERVED_PROFILE_SPEC.md` | observedIoProfile / validationHash 스펙 |
| `docs/SECURITY_SCAN_SPEC.md` | security referrer 스펙 |
| `docs/RUNNER_NODE_SPEC.md` | DagEdit RunnerNode 계약 |
| `docs/INFRALAB_TESTING.md` | K8s 배포 + e2e 테스트 절차 |
| `docs/INDEX_SCHEMA.md` | Index schema v3 |
