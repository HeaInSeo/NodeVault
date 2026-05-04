# Platform Schedule

버전: 2.1
작성일: 2026-04-18 / 갱신: 2026-05-04
참조 문서:
- `batch-integration/docs/master-plan/NodeKit/NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` (§14 Sprint 기간 및 완료 판정 기준)
- `docs/OBSERVED_PROFILE_SPEC.md`, `docs/SECURITY_SCAN_SPEC.md`, `docs/RUNNER_NODE_SPEC.md`, `docs/NODEVAULT_V03_MAPPING.md`

---

## 현재 운영 상태 (2026-05-04 기준)

| 컴포넌트 | 상태 |
|----------|------|
| NodeVault | seoy 호스트 바이너리 — `nodevault.service` active |
| NodePalette | seoy 호스트 바이너리 — `nodepalette.service` active |
| Harbor | harbor.10.113.24.96.nip.io 운영 중 |
| NodeKit | L1 + BuildRequest gRPC 완성, AdminToolList REST 완성 |
| DockGuard | 9개 규칙, .wasm 번들 완성 |
| sori | `PushToolProfileReferrer`, `PushSecurityReferrer` 추가, GitHub push 완료 |
| infra-lab | 클러스터 기동 중 — 통합 테스트 **미실행** |

---

## 완료된 마일스톤

| 완료일 | 항목 |
|--------|------|
| 2026-04-19 | TODO-06~11 (index, toolspec referrer, catalogrest, NodePalette 분리) |
| 2026-04-20 | TODO-09b 코드 — authority map 단일 write authority |
| 2026-04-28 | proto rename (nodevault.v1), go.work 제거 |
| 2026-05-02 | Path A — subuid/subgid 보정, kubeconfig 정책, 배포 자동화 |
| 2026-05-03 | **v0.6.1 Sprint 0 완료** — 형제 문서 6개 작성 |
| 2026-05-04 | sori — toolprofile/security referrer 함수 추가, readme 갱신 |

---

## 즉시 필요 (Sprint 1 시작 전 선행)

| 작업 | 소요 | 비고 |
|------|------|------|
| `make deploy-infralab` + `make test-integration-infralab` | 2~4시간 | 핸드오프 Priority 1, 미실행 |
| seoy e2e 확인 (`make deploy-seoy` + NodeKit → NodeVault 등록) | 1~2시간 | TODO-09b 완료 조건 |

---

## v0.6.1 Sprint 일정

설계 문서 §14 기준 — 총 7~8주 (Sprint 0 완료 기준).

### Sprint 0 — 계약 정렬 문서 ✓ (완료: 2026-05-03)

설계 문서 기준 1주. 실제 1주 이내 완료.

**산출물**: TOOL_CONTRACT_V0_3_DRAFT.md / OBSERVED_PROFILE_SPEC.md / SECURITY_SCAN_SPEC.md / RUNNER_NODE_SPEC.md / NODEVAULT_V03_MAPPING.md / TOOL_NODE_SPEC.md 갱신

---

### Sprint 1 — observed profile 기반 추가 (예상: 2주, ~2026-05-05~18)

**목표**: toolspec referrer 유지 + toolprofile referrer 별도 artifact 추가

**작업 목록**

| 파일 | 변경 내용 |
|------|-----------|
| `protos/nodevault/v1/nodevault.proto` | field 19~22: `authoring_hash`, `validation_hash`, `observed_profile_digest`, `security_scan_digest` |
| `pkg/index/schema.go:Entry` | 4개 optional field (`omitempty`) |
| `pkg/oras/referrer.go` | `PushToolProfileReferrer` (sori `PushToolProfileReferrer` wrapping) |
| `pkg/index/store.go` | `SetObservedProfileDigest`, `SetAuthoringHash` |
| `pkg/catalogrest` | REST 응답에 `observedProfileDigest`, `validationHash` 포함 |

**완료 판정 기준** (설계 문서 §14 Sprint 1)

- [ ] `MediaTypeToolProfile` 상수 코드 존재
- [ ] `TestPushToolProfileReferrer` 통과
- [ ] `TestDualReferrerCoexistence` — toolspec + toolprofile 공존
- [ ] `TestIndexMixedEntries_V04` — 신규 field 있는 entry + 기존 entry 혼재 로드
- [ ] `TestCasHashStability` 또는 `TestExistingToolDefinitionCasHashGolden` — 기존 casHash 불변
- [ ] `TestIndexBackwardCompatibility_V03Fields` — 신규 4개 field 없이 기존 entry 정상 로드
- [ ] `TestIndex_FallbackOnError` — atomic write 실패 시 기존 index 보존
- [ ] `go test ./...` 전체 통과
- [ ] `make lint` 경고 없음

---

### Sprint 2 — Validator/Profiler 연결 (예상: 2~3주, ~2026-05-18~06-08)

**목표**: Build/Register 흐름에 Validator/Profiler hook 연결, 최소 dry-run으로 observed I/O profile 생성

**최소 dry-run 기준** (설계 문서 §14 Sprint 2)
```
command: echo hello > /out/result.txt
expected: /out/result.txt exists=true, count=1, totalBytes>0
```

**작업 목록**

| 패키지/파일 | 내용 |
|-------------|------|
| `pkg/profiler/` (신규) | Profiler 패키지 — sample data 실행, output 수집 |
| `pkg/profiler/models.go` | `ValidationRun`, `ObservedIoProfile`, `ObservedResourceProfile`, `ContractCheck` |
| `pkg/profiler/hash.go` | `ValidationHash` 계산 (환경 독립 항목만 포함 — `OBSERVED_PROFILE_SPEC.md §3` 기준) |
| `pkg/profiler/classifier.go` | infra-level failure 분류 (OOMKilled, timeout, eviction 등) |
| `pkg/build/service.go` | Profiler hook 연결 — 등록 후 profile attach |
| `pkg/oras/referrer.go` | `PushToolProfileReferrer` 호출 |
| `pkg/index/store.go` | `SetObservedProfileDigest` 호출 |

**완료 판정 기준** (설계 문서 §14 Sprint 2)

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
- [ ] `TestIndex_FallbackOnError` 통과
- [ ] `go test ./...` 전체 통과

---

### Sprint 3 — DagEdit RunnerNode 연결 (예상: 2주, ~2026-06-08~22)

**목표**: NodeVault catalog → DagEdit RunnerNode까지 casHash 기반 pinning 실제 모델 연결

**작업 목록**

| 위치 | 내용 |
|------|------|
| `pkg/catalogrest` | REST 응답에 `observedProfileDigest`, `validationHash`, `toolspecReferrerDigest` 포함 (Sprint 1에서 일부 시작) |
| DagEdit repo | `RunnerNode` JSON schema — `casHash` 필수, 나머지 선택 |
| DagEdit repo | 팔레트 → NodePalette REST 조회 → `casHash` 기록 |
| DagEdit repo | UI badge 표시 (Verified / Unverified / No dry-run profile) |

**완료 판정 기준** (설계 문서 §14 Sprint 3)

- [ ] `TestRunnerNode_WithoutCasHash_Fails` — casHash 없는 RunnerNode 거부
- [ ] `TestRunnerNode_Serialization_RoundTrip` — casHash 직렬화 보존
- [ ] `TestRunnerNode_OptionalFields_Absent` — optional field 없이 동작
- [ ] `TestPortBinding_ParentToChild` — DAG edge 연결
- [ ] `TestRunnerNode_FromCatalogResponse` — catalog 응답 → RunnerNode 생성
- [ ] `TestNodePaletteBadge_DefaultsForMissingOptionalMetadata` — badge 기본값

**계약 문서**: `docs/RUNNER_NODE_SPEC.md`

---

### 병렬 트랙 A — Security Scan Integration (예상: 1~2주, Sprint 2 이후)

**목표**: CVE scan result를 toolprofile과 독립된 security referrer로 관리

**작업 목록**

| 파일/위치 | 내용 |
|-----------|------|
| `pkg/oras/referrer.go` | `PushSecurityReferrer` NodeVault wrapper |
| `pkg/reconcile/` | trivy-operator VulnerabilityReport CR 조회 + summary 추출 |
| `pkg/index/store.go` | `SetSecurityScanDigest` |
| `pkg/catalogrest` | security badge 응답 포함 |

**완료 판정 기준** (설계 문서 §14 Security Scan 병렬 트랙)

- [ ] `SECURITY_SCAN_SPEC.md` 존재 ✓ (완료)
- [ ] `MediaTypeSecurityScan` 상수 코드 존재 ✓ (sori 완료)
- [ ] `TestSecurityReport_FromTrivyVulnerabilityReport` 통과
- [ ] `TestSecurityReferrerPayload_Generate` 통과
- [ ] `TestIndexBackwardCompatibility_SecurityScanDigest` 통과
- [ ] `TestSecurityPolicy_RecordOnlyDoesNotBlockActive` 통과
- [ ] `TestSecurityRetention_MarksOldReferrersAsGCCandidates` 통과

**스펙**: `docs/SECURITY_SCAN_SPEC.md`

---

### 병렬 트랙 B — TODO-12 Data write path (예상: 2일, Sprint 1 이후 가능)

**목표**: NodeKit data registration UI → NodeVault gRPC 실제 전송 연결

| 작업 | 소요 |
|------|------|
| `DataRegisterRequestFactory.cs` → gRPC 실제 전송 | 1일 |
| NodeVault `DataRegistryService` 수신 + index 기록 검증 | 0.5일 |
| 통합 테스트 | 0.5일 |

---

## 전체 일정 요약

설계 문서 §14 기준: **Sprint 0 완료 후 7~8주**.

```
2026-05-04  ■ 현재 위치 (Sprint 0 완료)
            │
~05-07      ├── 선행: infra-lab 검증 + seoy e2e         [2~6시간]
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

---

## 공통 gate (모든 Sprint 적용)

설계 문서 §14 공통 gate:

- 기존 테스트가 깨지지 않는다
- 기존 `casHash` 계산 방식이 변경되지 않는다
- `assets/catalog/{casHash}.tooldefinition` 경로 의미가 유지된다
- 기존 index entry가 신규 optional field 없이도 정상 로드된다
- 신규 필드는 모두 backward-compatible optional field로 추가한다
- index update 중 오류가 발생해도 기존 index를 손상시키지 않는다

---

## 프로젝트별 상세 문서

| 항목 | 문서 |
|------|------|
| v0.6.1 전체 설계 | `batch-integration/docs/master-plan/NodeKit/NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` |
| NodeVault 전체 TODO | `docs/NODEVAULT_TRANSITION_PLAN.md` |
| v0.3 계약 | `docs/TOOL_CONTRACT_V0_3_DRAFT.md` |
| toolprofile referrer 스펙 | `docs/OBSERVED_PROFILE_SPEC.md` |
| security referrer 스펙 | `docs/SECURITY_SCAN_SPEC.md` |
| DagEdit RunnerNode 계약 | `docs/RUNNER_NODE_SPEC.md` |
| v0.6.1 용어 ↔ 코드 대응 | `docs/NODEVAULT_V03_MAPPING.md` |
| infra-lab 테스트 절차 | `docs/INFRALAB_TESTING.md` |
