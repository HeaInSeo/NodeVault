# Platform Master Design

버전: 1.0
작성일: 2026-05-04
상태: **정본 (Canon)** — 흩어진 문서를 단일 진실 원천으로 통합
갱신 책임: 이 문서가 변경되면 연결된 상세 문서도 함께 갱신한다

---

## 0. 이 문서의 목적과 사용법

플랫폼을 구성하는 여러 저장소에 설계 문서가 분산되어 있어 일관성 유지가 어렵다.
이 문서는 **플랫폼 전체의 단일 정본**이다.

- **개발 세션 시작 시** 이 문서 하나로 전체 맥락을 파악한다.
- **상세 규칙은 §10 문서 인덱스**의 개별 문서를 참조한다.
- **코드를 작성하기 전** §3 불변 아키텍처 결정과 CLAUDE.md를 확인한다.
- **새 기능을 시작하기 전** §7 스프린트 계획의 선행 조건을 확인한다.

---

## 1. 플랫폼 한 줄 정의

관리자가 생물정보학 툴 이미지를 정의·검증·빌드·등록하고,
파이프라인 빌더(DagEdit)가 그 툴을 가져다 재현 가능한 실행 파이프라인을 구성하는 플랫폼.

**핵심 원칙**: `same data + same method = same result`.
재현성은 비타협 원칙이며, bypass 경로는 존재하지 않는다.

---

## 2. 컴포넌트 지도

### 2.1 현재 운영 중인 컴포넌트

```
[관리자 (Administrator)]
    │ Tool 정의 + L1 검증 + DockGuard 정책 검사
    ▼
[NodeKit]  ── C#/Avalonia 데스크톱 클라이언트
    │ BuildRequest (gRPC :50051)    AdminToolList (REST :8080 GET)
    │ AdminDataList  (REST :8080 GET)
    ▼                                     ▲
[NodeVault]  ── Go, seoy 호스트 바이너리   │
    ├── BuildService     L2→L3→L4 + CAS 등록 + index 기록
    ├── PolicyService    DockGuard .wasm 번들 관리
    ├── pkg/index        artifact 상태 원장 (이중 축 상태 모델)
    ├── pkg/oras         OCI referrer push (sori 래핑)
    └── pkg/reconcile    Harbor 현실 대조 (FastRun/SlowRun)
    │
    ├── [NodePalette]  ── cmd/palette/, seoy 호스트 바이너리 (별도 프로세스)
    │       ├── HTTP :8080
    │       └── pkg/catalogrest → GET /v1/catalog/tools, /v1/catalog/data
    │
    ├── 이미지 push / Harbor spec referrer push
    ▼
[Harbor]  ── OCI 레지스트리 (harbor.10.113.24.96.nip.io)
    └── library/<tool>:latest
        └── [toolspec referrer]    ← application/vnd.nodevault.toolspec.v1+json
        └── [toolprofile referrer] ← application/vnd.nodevault.toolprofile.v1+json (Sprint 2~)
        └── [security referrer]    ← application/vnd.nodevault.security.v1+json  (병렬 트랙~)

[DockGuard]  ── OPA/Rego 정책 (9개 규칙, .wasm 번들)
    └── NodeKit WasmPolicyChecker가 로컬 실행 (L1)
    └── NodeVault PolicyService가 번들 배포

[sori]  ── Go OCI 라이브러리 (github.com/HeaInSeo/sori)
    └── NodeVault pkg/oras가 호출 — oras-go 직접 사용 금지
    └── PushToolSpecReferrer / PushToolProfileReferrer / PushSecurityReferrer
```

### 2.2 미래 컴포넌트 (설계됨, 미구현)

```
[DagEdit]  ── C#/Avalonia 파이프라인 빌더 (Stage 1 완성)
    └── NodePalette REST로 casHash 조회 → RunnerNode.casHash pin (Sprint 3~)

[NodeSentinel]  ── K8s data-plane 검증 서비스 (companion 설계 존재)
    └── L5-a Validator/Profiler를 K8s Job으로 분리 가능한 미래 경로
    └── 현재 NodeVault pkg/validate가 그 역할 유지
```

---

## 2. 저장소 및 경로

| 저장소 | 로컬 경로 | 언어 | 상태 |
|--------|-----------|------|------|
| NodeVault | `/opt/go/src/github.com/HeaInSeo/NodeVault` | Go | 운영 중 (seoy 호스트 바이너리) |
| NodeKit | `/opt/dotnet/src/github.com/HeaInSeo/NodeKit` | C# / Avalonia | 운영 중 |
| DockGuard | `/opt/dotnet/src/github.com/HeaInSeo/DockGuard` | OPA/Rego | 9개 규칙, .wasm 완성 |
| DagEdit | `/opt/dotnet/src/github.com/HeaInSeo/DagEdit` | C# / Avalonia | Stage 1 완성, NodePalette 미연결 |
| sori | `/opt/go/src/github.com/HeaInSeo/sori` | Go | 운영 중 (module path 갱신 진행 중) |
| infra-lab | `/opt/go/src/github.com/HeaInSeo/infra-lab` | Shell/YAML | 클러스터 기동 중 |
| batch-integration | `/opt/go/src/github.com/HeaInSeo/batch-integration` | Go | JUMI + artifact-handoff + kube-slint |
| NodeSentinel | `/opt/go/src/github.com/HeaInSeo/NodeSentinel` | Go | 설계만 존재 (미구현) |

---

## 3. 인프라 (seoy, 100.123.80.48)

| 항목 | 값 |
|------|-----|
| K8s 클러스터 | multipass VM 3노드 (lab-master-0, lab-worker-0, lab-worker-1) |
| CNI | Cilium |
| Harbor | `harbor.10.113.24.96.nip.io` (Cilium LB VIP 10.113.24.96) |
| NodeVault gRPC | `100.123.80.48:50051` (seoy 호스트 직접, Cilium GRPCRoute 미사용) |
| NodePalette REST | `http://100.123.80.48:8080` (seoy 호스트 직접) |
| Harbor admin | `Harbor12345` |
| kubeconfig 경로 | `/opt/go/src/github.com/HeaInSeo/infra-lab/kubeconfig` |

**배포 제약**: NodeVault는 **seoy 호스트 바이너리**로 실행한다 (K8s Pod 아님).
이유: podbridge5(buildah) rootless 제약 — K8s Pod 안에서 overlay 마운트 불가.
K8s 접근은 로컬 kubeconfig 경유로 L3/L4 Job 제출에만 사용한다.

Harbor 접근 시 라우트 (필요시):
```bash
ip route add 10.113.24.96/32 via 10.113.24.254
```

---

## 4. 불변 아키텍처 결정

이 섹션의 결정은 명시적 아키텍처 논의 없이 변경할 수 없다.

### 4.1 재현성 (Non-Negotiable)

| 규칙 | 내용 |
|------|------|
| `latest` 태그 금지 | L1에서 무조건 차단. bypass 플래그 없음 |
| digest 미고정 금지 | 이미지 URI에 digest 또는 버전 필수 |
| 버전 미고정 패키지 금지 | package dependency에 unpinned 버전 차단 |
| casHash 실행 pin | 파이프라인 노드는 반드시 casHash로 실행을 pin |

### 4.2 casHash 정의 (불변)

```
casHash = SHA256(spec JSON without cas_hash field)
```

- **계산 방식 및 의미 절대 변경 금지**
- 파이프라인 저장·실행의 유일한 식별자
- 새로운 hash 개념이 필요하면 별도 이름 도입 (`runtimeProfileHash` 등)
- `validationHash`는 casHash와 다른 목적 — functional validation 결과 hash

### 4.3 stableRef 경계

| 용도 | 축 |
|------|-----|
| UI 탐색·표시 | `stableRef` (`tool_name@version`) |
| pipeline 저장·실행 | `casHash` (SHA256 digest) |

- stableRef cardinality: **1:N** — 같은 stableRef에 여러 casHash 허용, 동시 Active 허용
- DagEdit RunnerNode는 `casHash` 필수, `stableRef`는 displaySnapshot용 선택

### 4.4 artifact 상태 이중 축 (절대 혼합 금지)

| 축 | 값 | 변경 주체 | 의미 |
|----|-----|-----------|------|
| `lifecycle_phase` | Pending / Active / Retracted / Deleted | **NodeVault 명시적 호출만** | 관리자의 승인 의도 |
| `integrity_health` | Healthy / Partial / Missing / Unreachable / Orphaned | **reconcile loop만** | Harbor 현실 대조 결과 |

**NodePalette 노출 기준**: `lifecycle_phase = Active`인 항목만. `integrity_health`는 알람/모니터링 전용.

교차 상태 예시:

| lifecycle_phase | integrity_health | 처리 |
|-----------------|------------------|------|
| Active | Healthy | Catalog 노출, 정상 |
| Active | Partial | Catalog 노출, spec referrer 없음 알람 |
| Active | Missing | Catalog 노출 유지, 긴급 조사 알람 |
| Retracted | * | Catalog 제외, Harbor GC 대기 |
| Deleted | * | 모든 경로에서 제외 |

`lifecycle_phase` 전이 규칙:
```
Pending  ──[L4 통과 + RegisterTool]──▶ Active
Active   ──[운영자 Retract]──────────▶ Retracted
Retracted ─[운영자 Restore]──────────▶ Active
Retracted ─[운영자 Delete + Harbor GC]▶ Deleted
```
금지 전이: `Pending → Retracted`, `Active → Deleted` (반드시 Retracted 경유)

### 4.5 Write Authority Map

| 작업 | 소유자 |
|------|--------|
| Build Job 실행 | NodeVault (위임) |
| `lifecycle_phase` 변경 | **NodeVault 명시적 호출만** |
| `integrity_health` 변경 | **reconcile loop만** |
| Index append / read | `pkg/index` 패키지 경유만 (직접 파일 접근 금지) |
| Catalog 노출 결정 | `lifecycle_phase = Active` 기준만 |
| Retract / Delete | NodeVault 명시적 호출만 |

### 4.6 OCI Referrer 분리

등록 시점이 다른 metadata는 별도 referrer artifact로 분리한다.
하나의 artifact에 묶으면 한 축만 갱신되어도 다른 축까지 교체해야 한다.

| artifactType | 책임 | 갱신 시점 | 갱신 주체 | 상태 |
|---|---|---|---|---|
| `application/vnd.nodevault.toolspec.v1+json` | declared spec (ToolDefinition, PortSpec, runtime, license, sourceEvidence) | 등록 1회 | NodeVault pkg/oras | 구현 완료 |
| `application/vnd.nodevault.toolprofile.v1+json` | observed dry-run profile (validationRun, observedIoProfile, observedResourceProfile, contractCheck, validationHash) | dry-run 재실행 시 | Validator/Profiler (Sprint 2~) | Sprint 1에서 상수+wrapper 추가 |
| `application/vnd.nodevault.security.v1+json` | security scan summary (scanner identity, VulnerabilityReport digest, severity summary, policy result) | CVE DB 갱신 시 | trivy-operator → NodeVault | 병렬 트랙에서 구현 |

### 4.7 sori 통합 경계

- NodeVault는 oras-go를 **직접 호출하지 않는다** — 반드시 sori를 경유
- sori는 NodeVault 내부 타입(index.Store, index.Entry, lifecycle_phase 등)을 모른다
- 호출 방향: `NodeVault → sori → Harbor/registry` (역방향 없음)
- sori 기능: OCI store/repository 생성, specJSON 직렬화, referrer push (프로토콜 전담)
- NodeVault 기능: 언제/무엇을 push할지 결정, index 기록 (의미 전담)

### 4.8 image build 방식

NodeVault 바이너리가 podbridge5를 **in-process로 직접 실행** (seoy 호스트에서).
K8s Job으로 위임하지 않는다 — podbridge5(buildah) rootless 제약으로 K8s Pod 안에서 overlay 마운트 불가.

---

## 5. 검증 계층 (L1~L5)

| 계층 | 이름 | 담당 | 상태 |
|------|------|------|------|
| L1 | 정적 검증 + DockGuard 정책 | NodeKit WasmPolicyChecker (DockGuard .wasm 로컬 실행) | 구현 완료 |
| L2 | 이미지 빌드 | NodeVault podbridge5 in-process | 구현 완료 |
| L3 | K8s dry-run (Job manifest 검증) | NodeVault pkg/validate | 구현 완료 |
| L4 | K8s smoke run (컨테이너 실행 확인) | NodeVault pkg/validate | 구현 완료 |
| L5-a | Validator/Profiler (observed I/O profile + validationHash) | NodeVault pkg/profiler (Sprint 2~) 또는 NodeSentinel | Sprint 2에서 구현 |
| L5-b | Security Scan (CVE summary + policy) | trivy-operator → NodeVault pkg/reconcile | 병렬 트랙에서 구현 |

### 5.1 validationHash 운영 규칙 (L5-a)

**생성 조건**: Successful functional validation에서만.

**hash 입력에 포함**: `mode`, `imageDigest`, `runnerScriptDigest`, `sampleDataRefs digest`, `command`, `exitCode`, I/O 결정론적 요약(file exists, count, nonEmpty, comparator result), `contractCheck summary`

**hash 입력에서 제외**: `peakCPU`, `avgCPU`, `peakMemory`, `durationSeconds`, disk I/O bytes, node name, cpu model, total memory, raw stdout/stderr, timestamp (환경 종속 값은 모두 제외)

**Infra-level failure 시 미생성**: OOMKilled, timeout, eviction, scheduling failure, image pull failure, SIGTERM/SIGKILL → `validationStatus: "infra_failed"`, `profileStatus: "inconclusive"`

**Application-level failure**: 기본 정책 미생성. `expected-failure fixture`가 있을 때만 정책 옵션으로 허용.

---

## 6. end-to-end 흐름 (Tool 빌드 happy path)

```
1. NodeKit AuthoringPanel: ToolDefinition 작성
   ├── L1 정적 검증 (RequiredFields / ImageUri / PackageVersion)
   └── DockGuard .wasm 정책 검사 (WasmPolicyChecker)

2. BuildRequest → gRPC :50051 → NodeVault BuildService
   ├── L2: podbridge5 in-process 이미지 빌드 → Harbor push → digest 획득
   ├── L3: K8s Job dry-run (Job manifest 검증)
   └── L4: K8s smoke run (실제 컨테이너 실행 확인)

3. 등록
   ├── pkg/catalog: CAS JSON 저장 (assets/catalog/{casHash}.tooldefinition)
   ├── pkg/index: vault-index.json append
   │     lifecycle_phase = Active, integrity_health = Partial
   └── pkg/oras → sori.PushToolSpecReferrer() → Harbor
         integrity_health = Healthy (referrer 첨부 완료)

4. BuildEvent 스트림 → NodeKit 빌드 로그 표시

5. NodeKit AdminToolList
   └── GET /v1/catalog/tools → NodePalette → index.Store.ListActive()

[Sprint 2 이후 — Validator/Profiler hook]
6. pkg/profiler: 등록 후 dry-run profile 실행
   └── sori.PushToolProfileReferrer() → Harbor toolprofile referrer
   └── index.Entry.ObservedProfileDigest 갱신

[병렬 트랙 이후 — Security Scan]
7. trivy-operator VulnerabilityReport CR 조회 → summary 추출
   └── sori.PushSecurityReferrer() → Harbor security referrer
   └── index.Entry.SecurityScanDigest 갱신
```

---

## 7. v0.6.1 스프린트 계획

설계 근거: `batch-integration/docs/master-plan/NodeKit/NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md`

총 기간: Sprint 0 완료(2026-05-03) 기준 7~8주.

```
2026-05-04  ■ 현재 위치 (Sprint 0 완료)
            │
~05-07      ├── 선행: infra-lab 검증 + seoy e2e          [2~6시간]
            │
05-05~18    ├── Sprint 1  additive field + toolprofile referrer  [2주]
            │
05-18~06-08 ├── Sprint 2  Validator/Profiler                     [2~3주]
            │   └── 병렬 트랙 B: TODO-12 Data write path         [2일]
            │
06-08~06-22 └── Sprint 3  DagEdit RunnerNode                     [2주]
                └── 병렬 트랙 A: Security Scan                   [1~2주]

총 완료 예상: 2026-06-22 (Sprint 3) / Security Scan 포함 시 ~06-29
```

### 7.1 Sprint 0 — 계약 정렬 문서 ✓ (완료: 2026-05-03)

산출물 6개:
- `docs/TOOL_CONTRACT_V0_3_DRAFT.md` — v0.3 additive field 계약
- `docs/OBSERVED_PROFILE_SPEC.md` — toolprofile referrer payload + validationHash 규칙
- `docs/SECURITY_SCAN_SPEC.md` — security referrer payload + trivy-operator 통합
- `docs/RUNNER_NODE_SPEC.md` — DagEdit RunnerNode contract
- `docs/NODEVAULT_V03_MAPPING.md` — v0.6.1 vocabulary ↔ NodeVault 코드 위치 포인터
- `docs/TOOL_NODE_SPEC.md` Layer 5 갱신

### 7.2 Sprint 1 — additive field 코드 추가 (예상: 2026-05-05~18)

**목표**: toolspec referrer 유지 + toolprofile referrer 별도 artifact 추가

| 파일 | 변경 내용 |
|------|-----------|
| `protos/nodevault/v1/nodevault.proto` | field 19~22: `authoring_hash`, `validation_hash`, `observed_profile_digest`, `security_scan_digest` |
| `pkg/index/schema.go:Entry` | 4개 optional field (`omitempty`) |
| `pkg/oras/referrer.go` | `PushToolProfileReferrer` (sori 래핑) |
| `pkg/index/store.go` | `SetObservedProfileDigest`, `SetAuthoringHash` |
| `pkg/catalogrest` | REST 응답에 `observedProfileDigest`, `validationHash` 포함 |

**완료 판정 기준** (모두 통과해야 Sprint 1 완료):
- [ ] `MediaTypeToolProfile` 상수 코드 존재
- [ ] `TestPushToolProfileReferrer` 통과
- [ ] `TestDualReferrerCoexistence` — toolspec + toolprofile 공존
- [ ] `TestIndexMixedEntries_V04` — 신규 field 있는 entry + 기존 entry 혼재 로드
- [ ] `TestCasHashStability` 또는 `TestExistingToolDefinitionCasHashGolden` — 기존 casHash 불변
- [ ] `TestIndexBackwardCompatibility_V03Fields` — 신규 4개 field 없이 기존 entry 정상 로드
- [ ] `TestIndex_FallbackOnError` — atomic write 실패 시 기존 index 보존
- [ ] `go test ./...` 전체 통과
- [ ] `make lint` 경고 없음

### 7.3 Sprint 2 — Validator/Profiler 연결 (예상: 2026-05-18~06-08)

**목표**: Build/Register 흐름에 Validator/Profiler hook 연결, 최소 dry-run으로 observed I/O profile 생성

**최소 dry-run 기준**:
```
command: echo hello > /out/result.txt
expected: /out/result.txt exists=true, count=1, totalBytes>0
```

| 패키지/파일 | 내용 |
|-------------|------|
| `pkg/profiler/` (신규) | ValidationRun, ObservedIoProfile, ObservedResourceProfile, ContractCheck |
| `pkg/profiler/hash.go` | ValidationHash 계산 (환경 독립 항목만, OBSERVED_PROFILE_SPEC.md §3 기준) |
| `pkg/profiler/classifier.go` | infra-level failure 분류 (OOMKilled, timeout, eviction 등) |
| `pkg/build/service.go` | Profiler hook 연결 — 등록 후 profile attach |
| `pkg/oras/referrer.go` | `PushToolProfileReferrer` 호출 |
| `pkg/index/store.go` | `SetObservedProfileDigest` 호출 |

**완료 판정 기준**:
- [ ] `go build ./pkg/profiler/...` 성공
- [ ] `TestBuildAndRegister_ProfilerHookCalled` 통과
- [ ] `TestProfiler_OutputCapture` 통과
- [ ] `TestValidationHash_Deterministic` 통과
- [ ] `TestValidationHash_ExcludesObservedResourcesByDefault` 통과
- [ ] `TestValidationHash_OnlyForSuccessfulFunctionalValidation` 통과
- [ ] `TestValidator_InfraFailureClassification` 통과
- [ ] `TestProfiler_TimeoutProducesInconclusiveProfile` 통과
- [ ] `TestBuildAndRegister_WithProfile` 통합 테스트 통과
- [ ] `TestCasHashStability` 통과 (기존 casHash 불변 재확인)
- [ ] `go test ./...` 전체 통과

### 7.4 Sprint 3 — DagEdit RunnerNode 연결 (예상: 2026-06-08~22)

**목표**: NodeVault catalog → DagEdit RunnerNode까지 casHash 기반 pinning 실제 모델 연결

| 위치 | 내용 |
|------|------|
| `pkg/catalogrest` | REST 응답에 `observedProfileDigest`, `validationHash`, `toolspecReferrerDigest` 포함 |
| DagEdit repo | `RunnerNode` JSON schema — `casHash` 필수, 나머지 선택 |
| DagEdit repo | 팔레트 → NodePalette REST 조회 → `casHash` 기록 |
| DagEdit repo | UI badge 표시 (Verified / Unverified / No dry-run profile) |

**완료 판정 기준**:
- [ ] `TestRunnerNode_WithoutCasHash_Fails`
- [ ] `TestRunnerNode_Serialization_RoundTrip`
- [ ] `TestRunnerNode_OptionalFields_Absent`
- [ ] `TestPortBinding_ParentToChild`
- [ ] `TestRunnerNode_FromCatalogResponse`
- [ ] `TestNodePaletteBadge_DefaultsForMissingOptionalMetadata`

### 7.5 병렬 트랙 A — Security Scan Integration (Sprint 2 이후, ~1~2주)

| 파일/위치 | 내용 |
|-----------|------|
| `pkg/oras/referrer.go` | `PushSecurityReferrer` NodeVault wrapper |
| `pkg/reconcile/` | trivy-operator VulnerabilityReport CR 조회 + summary 추출 |
| `pkg/index/store.go` | `SetSecurityScanDigest` |
| `pkg/catalogrest` | security badge 응답 포함 |

**완료 판정 기준**:
- [ ] `TestSecurityReport_FromTrivyVulnerabilityReport`
- [ ] `TestSecurityReferrerPayload_Generate`
- [ ] `TestIndexBackwardCompatibility_SecurityScanDigest`
- [ ] `TestSecurityPolicy_RecordOnlyDoesNotBlockActive`
- [ ] `TestSecurityRetention_MarksOldReferrersAsGCCandidates`

### 7.6 병렬 트랙 B — TODO-12 Data write path (Sprint 1 이후, ~2일)

| 작업 | 소요 |
|------|------|
| `DataRegisterRequestFactory.cs` → gRPC 실제 전송 | 1일 |
| NodeVault `DataRegistryService` 수신 + index 기록 검증 | 0.5일 |
| 통합 테스트 | 0.5일 |

---

## 8. 즉시 선행 작업 (Sprint 1 시작 전)

| 작업 | 소요 | 비고 |
|------|------|------|
| `make deploy-infralab` + `make test-integration-infralab` | 2~4시간 | 핸드오프 Priority 1, 미실행 |
| seoy e2e 확인 (`make deploy-seoy` + NodeKit → NodeVault 등록) | 1~2시간 | TODO-09b 완료 조건 |
| sori module path 완성 (내부 import `seoyhaein` → `HeaInSeo`) | ~1시간 | go.mod만 변경됨, 내부 파일 미변경 |
| NodeVault vendor 갱신 (`go get github.com/HeaInSeo/sori@master`) | ~30분 | module path 완성 후 |

---

## 9. 공통 Gate (모든 Sprint 적용)

- 기존 테스트가 깨지지 않는다
- 기존 `casHash` 계산 방식이 변경되지 않는다
- `assets/catalog/{casHash}.tooldefinition` 경로 의미가 유지된다
- 기존 index entry가 신규 optional field 없이도 정상 로드된다
- 신규 필드는 모두 backward-compatible optional field로 추가한다
- index update 중 오류가 발생해도 기존 index를 손상시키지 않는다
- `make lint` 경고 없음

---

## 10. 비목표 (현재 버전에서 의도적으로 하지 않는 것)

| ID | 비목표 | 이유 |
|----|--------|------|
| N-01 | Harbor 외부 레지스트리 지원 | 재현성 정책은 플랫폼 통제 레지스트리에서만 보장 가능 |
| N-02 | 다중 클러스터 지원 | 단일 kubeconfig + 단일 클러스터 범위 |
| N-03 | Orphaned 상태 자동 삭제 | 수동 대기 + 알람 기본 정책. 자동 삭제는 운영 사고 위험 |
| N-04 | DagEdit 내부 타입 import | DagEdit는 별도 프로젝트 트랙. NodeVault는 Catalog REST API만 노출 |
| N-05 | `latest` 태그 허용 모드 | 재현성 비타협. bypass 플래그 없음 |
| N-06 | 파이프라인 실행 엔진 | NodeVault는 artifact 등록·관리만. 실행은 별도 엔진 |
| N-07 | stableRef current active 단수 포인터 | TODO-16b(UI revision 정책) 결정 전까지 설계 불가 |
| N-08 | NodeVault 런타임 K8s 전환 (현재) | Cilium + Harbor 안정화 전 착수 금지 |
| N-09 | Data artifact 등록 (현재) | index 스키마에 자리 확보, 구현은 병렬 트랙 B |
| N-10 | Harbor webhook-first 아키텍처 | Harbor GC 이벤트는 webhook 표면에 없음. reconcile-first가 기본 |

---

## 11. 현재 운영 상태 (2026-05-04 기준)

| 컴포넌트 | 상태 |
|----------|------|
| NodeVault | seoy 호스트 바이너리 — `nodevault.service` active |
| NodePalette | seoy 호스트 바이너리 — `nodepalette.service` active |
| Harbor | harbor.10.113.24.96.nip.io 운영 중 |
| NodeKit | L1 + BuildRequest gRPC 완성, AdminToolList REST 완성 |
| DockGuard | 9개 규칙, .wasm 번들 완성 |
| sori | PushToolProfileReferrer, PushSecurityReferrer 추가, GitHub push 완료 |
| infra-lab | 클러스터 기동 중 — 통합 테스트 **미실행** |
| DagEdit | Stage 1 완성 — NodePalette 미연결 (Sprint 3 이후) |
| NodeSentinel | 설계 문서만 존재 — 미구현 |

### 알려진 운영 중 경고 (무해, 빌드 파이프라인 완성 전 해결 필요)

| # | 증상 | 원인 | 해결 조건 |
|---|------|------|-----------|
| D-01 | `no subuid ranges found for user "nodevault"` | seoy에서 nodevault 서비스 계정 subuid/subgid 범위 미설정 | `scripts/deploy-seoy.sh`가 보정. 수동: `sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 nodevault` |
| D-02 | `ValidateService unavailable (kubeconfig missing?)` | `/opt/nodevault/kubeconfig` 파일 없음 | `scripts/deploy-seoy.sh`의 `KUBECONFIG_MODE=remote`로 배포 |

---

## 12. 문서 인덱스

### NodeVault 상세 문서

| 문서 | 위치 | 내용 |
|------|------|------|
| ARCHITECTURE.md | `docs/` | NodeVault 컴포넌트 구조, 실제 구현 기준 |
| NODEVAULT_TRANSITION_PLAN.md | `docs/` | **전체 TODO 목록 + 완료 현황** |
| PLATFORM_SCHEDULE.md | `docs/` | Sprint별 상세 일정 + 완료 기준 테스트 목록 |
| TOOL_CONTRACT_V0_2.md | `docs/` | RegisteredTool v0.2 확정 계약 (변경 금지) |
| TOOL_CONTRACT_V0_3_DRAFT.md | `docs/` | RegisteredTool v0.3 additive field 계약 (Sprint 0 산출물) |
| INDEX_SCHEMA.md | `docs/` | index 스키마 + 이중 축 상태 모델 |
| AUTHORITY_MAP.md | `docs/` | write authority 분리 설계 |
| NONGOALS.md | `docs/` | 비목표 목록 |
| OBSERVED_PROFILE_SPEC.md | `docs/` | toolprofile referrer payload + validationHash 운영 규칙 |
| SECURITY_SCAN_SPEC.md | `docs/` | security referrer payload + trivy-operator 통합 |
| RUNNER_NODE_SPEC.md | `docs/` | DagEdit RunnerNode contract |
| NODEVAULT_V03_MAPPING.md | `docs/` | v0.6.1 vocabulary ↔ NodeVault 코드 위치 포인터 |
| TOOL_NODE_SPEC.md | `docs/` | 툴 노드 YAML/JSON 5계층 현황 |
| SORI_INTEGRATION_BOUNDARY.md | `docs/` | sori 통합 경계 — 호출 방향 및 API 계약 |
| NODEPALETTE_DESIGN.md | `docs/` | NodePalette REST 상세 설계 |
| INFRALAB_TESTING.md | `docs/` | infra-lab 테스트 절차 |
| CATALOG_CACHE_STRATEGY.md | `docs/` | Catalog 캐시 전략 |
| DEPLOY_IN_CLUSTER.md | `docs/` | K8s 배포 절차 (미래 in-cluster 전환) |
| HARBOR_WEBHOOK_EVENTS.md | `docs/` | Harbor webhook 이벤트 목록 |
| TRUENAS_NFS_RUNBOOK.md | `docs/` | TrueNAS NFS 운영 절차 |

### NodeKit 상세 문서

| 문서 | 위치 | 내용 |
|------|------|------|
| CLAUDE.md | `NodeKit/` | **책임 경계, 재현성 규칙, 결정 체크리스트** |
| ARCHITECTURE.md | `NodeKit/docs/` | NodeKit 컴포넌트 레이어, 외부 연결 |

### 설계 마스터 문서

| 문서 | 위치 | 내용 |
|------|------|------|
| NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md | `batch-integration/docs/master-plan/NodeKit/` | **v0.6.1 전체 설계 (근거 문서)** |
| NodeSentinel_Validation_Data_Plane_설계_v0.1.md | `batch-integration/docs/master-plan/NodeKit/` | NodeSentinel K8s data-plane 설계 |
| ARCHITECTURE_OVERVIEW_v1.0.md | `batch-integration/docs/master-plan/` | 전체 플랫폼 아키텍처 개요 |
| MILESTONES_AND_GATES.md | `batch-integration/docs/master-plan/` | 마일스톤 및 완료 게이트 |

---

## 13. 책임 경계 요약

| 컴포넌트 | 소유하는 것 | 소유하지 않는 것 |
|----------|-------------|-----------------|
| NodeKit | 관리자 UX, L1 정적 검증, DockGuard WasmPolicyChecker, BuildRequest 생성, AdminToolList/AdminDataList 표시 | 이미지 빌드, index 관리, OCI referrer push |
| NodeVault | L2 빌드(podbridge5), L3/L4 K8s 검증, CAS 저장, index 관리(이중 축), OCI referrer push(sori 경유), DockGuard .wasm 번들 관리, NodePalette REST(cmd/palette) | 관리자 UX, L1 검증, DagEdit 내부 타입 |
| DockGuard | OPA/Rego 정책 규칙(9개), .wasm 번들 생성 | 정책 적용 실행(NodeKit WasmPolicyChecker 담당) |
| sori | OCI store/repository 연결, spec/profile/security referrer push (프로토콜 전담) | artifact 의미(index 관리, casHash, lifecycle_phase 등) |
| DagEdit | DAG 파이프라인 편집 UI, RunnerNode 모델 | 툴 이미지 관리, Catalog 관리 |
| infra-lab | K8s 클러스터 생성/관리, Harbor 배포 | 애플리케이션 로직 |
