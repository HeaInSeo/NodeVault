# RegisteredTool v0.3 계약 (Draft — Sprint 0)

버전: v0.3.0-draft
작성일: 2026-05-03
상태: **Draft (Sprint 0 — v0.6.1)**
선행: `TOOL_CONTRACT_V0_2.md` (확정 계약, 변경하지 않음)
근거 설계: `batch-integration/docs/master-plan/NodeKit/NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md`

---

## 0. 이 문서의 위치

이 문서는 v0.6.1 Sprint 0의 **계약 정렬 산출물**이다. 코드는 변경하지 않는다. 다음 Sprint(코드 변경 단계)에서 본 문서가 명시한 additive field만 proto/index/catalog에 반영한다.

- v0.6.1 Sprint 0 = 문서/계약 정렬 (이 문서 + 형제 5개 doc)
- v0.6.1 Sprint 1 = `toolprofile` referrer + `validationHash`/`observedProfileDigest` optional field 코드 추가
- v0.6.1 Sprint 2 = Validator/Profiler 패키지 + Build/Register profiler hook
- v0.6.1 Sprint 3 = DagEdit RunnerNode 모델 (DagEdit repo)
- 병렬 트랙 = Security Scan, Legacy import, External reference scan

---

## 1. 핵심 원칙 (v0.2 유지 + v0.3 추가)

### 1.1 v0.2에서 유지하는 것 (변경 금지)

- `stableRef` — 사람이 검색·탐색에 사용하는 이름 (`tool_name@version`). UI 전용.
- `casHash` — 시스템이 실행 pin에 사용하는 불변 식별자. `SHA256(spec JSON without cas_hash)`. 계산 방식·의미 모두 변경하지 않는다.
- 파이프라인 저장·실행은 **항상 casHash** 기준.
- 상태 이중 축 (`lifecycle_phase` / `integrity_health`) — 변경 주체 분리 유지.
- Catalog 노출 조건: `lifecycle_phase = Active`만.
- 기존 PortSpec 필드 7개 (`name`/`role`/`format`/`shape`/`required`/`class`/`constraints`) — 이름·타입·JSON tag 모두 그대로.

### 1.2 v0.3에서 추가하는 것 (additive only)

- 4 신규 optional 필드: `authoringHash` / `validationHash` / `observedProfileDigest` / `securityScanDigest`
- PortSpec additive 확장 후보: `staging`
- 신규 OCI referrer artifact type 2개: `toolprofile`, `security` (기존 `toolspec`은 유지)

> **`casHash` 재정의 금지.** 향후 fully-validated runtime profile identity가 필요하면 별도 이름(`runtimeProfileHash` / `validatedProfileHash`)을 도입한다. 새 hash는 본 문서 범위 밖.

---

## 2. 신규 추가 필드 (additive optional)

모든 신규 필드는 `optional`이며 JSON 직렬화 시 `omitempty`. 기존 entry는 새 필드 없이도 정상 로드되어야 한다.

| 필드 | 타입 | 출처 | 의미 | 기존 `casHash` 영향 |
|------|------|------|------|---------------------|
| `authoring_hash` | string | NodeKit / legacy-import | authoring request 또는 import source lineage 추적 | 없음 (additive) |
| `validation_hash` | string | Validator/Profiler (Sprint 2~) | successful functional validation summary hash | 없음 (additive) |
| `observed_profile_digest` | string | toolprofile referrer push (Sprint 1~) | 최신 `application/vnd.nodevault.toolprofile.v1+json` referrer digest | 없음 (additive) |
| `security_scan_digest` | string | security referrer push (병렬 Security 트랙) | 최신 `application/vnd.nodevault.security.v1+json` referrer digest | 없음 (additive) |

### 2.1 `validation_hash` 운영 규칙 (v0.6.1 §4.3)

- **Successful functional validation에 대해서만 생성**한다.
- Infra-level failure(OOMKilled, timeout, eviction, scheduling failure, image pull failure, SIGTERM/SIGKILL)에서는 생성하지 않는다.
- 환경 종속 측정값(`peak/avg CPU`, `peak memory`, `durationSeconds`, `disk read/write`, `node name`, `cpu model`)은 hash 입력에서 제외한다.
- Application-level failure(tool 자체가 정상 exit code 1/2 보고)는 기본 정책에서 hash 미생성. `expected-failure fixture`가 있는 경우에만 정책 옵션으로 허용.

상세는 형제 문서 `OBSERVED_PROFILE_SPEC.md` §3.

### 2.2 `observed_profile_digest` / `security_scan_digest` 캐시 정책 (v0.6.1 §5.4)

- index.Entry에는 **latest digest 1개만** 캐시한다.
- 과거 referrer artifact는 즉시 삭제하지 않고 GC candidate로 표시한다 (registry GC 정책에 위임).
- Retention 기본값: toolprofile = latest 3개, security = latest 3개 또는 최근 30일.

상세 정책은 `OBSERVED_PROFILE_SPEC.md` §5와 `SECURITY_SCAN_SPEC.md` §4.

---

## 3. PortSpec 확장 (additive only)

기존 PortSpec 7개 필드는 변경하지 않는다. 다음 필드만 additive로 추가하는 것을 v0.3에서 제안한다.

| 필드 | 타입 | 의미 | 기본값 |
|------|------|------|--------|
| `staging` | string | 입출력 데이터 staging 방식 (`file` / `stream` / `directory` / 향후 확장) | `"file"` (관례 동작과 동일) |

운영 규칙:

- 기존 PortSpec은 `staging` 없이도 유효하다. 누락 시 기본값 `"file"`로 해석한다.
- `format` 정규화(canonical enum / alias normalization)는 본 Sprint 범위 밖. v0.6.1 §16.4 open question.
- `shape`는 기존 enum 그대로 유지(`single` / `pair` 등). v0.6.1에서 `multiplicity` 의미와 매핑 (NODEVAULT_V03_MAPPING.md 참조).

---

## 4. 전체 필드 목록 (v0.2 + v0.3 additive)

기존 v0.2 필드는 `TOOL_CONTRACT_V0_2.md` §2 그대로 유지. 본 문서는 추가분만 명시한다.

| 필드 | 타입 | 출처 | 추가 시점 | 비고 |
|------|------|------|----------|------|
| `authoring_hash` | string (optional) | NodeKit / legacy-import | Sprint 1~ | additive, omitempty |
| `validation_hash` | string (optional) | Validator/Profiler | Sprint 2~ | successful validation only |
| `observed_profile_digest` | string (optional) | toolprofile referrer push | Sprint 1~ | latest only |
| `security_scan_digest` | string (optional) | security referrer push | 병렬 Security | latest only |

> 위 4개 필드는 `RegisteredToolDefinition` proto / `pkg/index.Entry` / catalog CAS JSON 세 곳 모두에 동일 JSON tag로 추가한다 (Sprint 1 코드 변경에서). 본 Sprint 0에서는 **선언만** 한다.

---

## 5. OCI Referrer 분리

기존 `toolspec` 한 개에 모든 metadata를 묶지 않는다. 시점·생명주기·갱신 주체가 다른 metadata는 별도 referrer artifact로 분리한다.

| Referrer artifactType | 책임 | 갱신 주기 | 갱신 주체 | 본 Sprint 0 상태 |
|------------------------|------|-----------|-----------|------------------|
| `application/vnd.nodevault.toolspec.v1+json` | declared spec (ToolDefinition / PortSpec / declared runtime / license / sourceEvidence / target platforms / runtime formats) | 등록 시점 1회 | NodeVault `pkg/oras` (기존 TODO-07) | **유지** — payload 변경 없음 |
| `application/vnd.nodevault.toolprofile.v1+json` | observed dry-run profile (validationRun / observedIoProfile / observedResourceProfile / contractCheck / validationHash) | dry-run 재실행 시 | Validator/Profiler (Sprint 2~) 또는 NodeSentinel | **신규 (스펙은 OBSERVED_PROFILE_SPEC.md)** |
| `application/vnd.nodevault.security.v1+json` | security scan summary (scanner identity / VulnerabilityReport digest / severity summary / freshness / policy result) | 재스캔 시 (CVE DB 갱신) | trivy-operator → NodeVault 또는 NodeSentinel aggregator | **신규 (스펙은 SECURITY_SCAN_SPEC.md)** |

분리 이유 (v0.6.1 §3.1, §3.2):

- `toolspec`은 등록 시점 declared metadata. 변경되지 않는다.
- `toolprofile`은 dry-run 결과 observed metadata. 같은 image라도 fixture/profile 정책 변경 시 갱신.
- `security`는 CVE DB · scanner version에 따라 결과가 시간에 따라 바뀐다. 다른 두 referrer와 독립 갱신.
- 한 artifact에 모두 넣으면 한 축만 갱신되어도 다른 축까지 교체해야 함.

---

## 6. 기존 호환성 (절대 깨지 않는 것)

본 v0.3 draft는 다음을 보장한다.

- 기존 `casHash` 계산 함수(`SHA256(spec JSON without cas_hash)`)와 결과값 변경 없음.
- 기존 `assets/catalog/{casHash}.tooldefinition` 경로 의미 유지. 파일 위치/형식 변경 없음.
- 기존 `pkg/index.Entry` JSON tag 유지. 신규 필드는 `omitempty`로만 추가.
- 기존 `nodevault.v1.RegisteredToolDefinition` proto field 번호 유지. 신규 필드는 새 번호로만 추가.
- 기존 PortSpec 7개 필드 이름/타입/번호 유지.
- 기존 단위/통합 테스트 모두 그린 유지.

검증은 Sprint 1 이후 코드 변경에서 다음 테스트로 강제한다:

- `TestCasHashStability` 또는 `TestExistingToolDefinitionCasHashGolden` — 기존 spec JSON으로 casHash 재계산이 골든값과 일치
- `TestIndexBackwardCompatibility_V03Fields` — 신규 4개 필드 없이 기존 entry 정상 로드
- `TestDualReferrerCoexistence` — toolspec + toolprofile 동시 attach
- `TestTripleReferrerCoexistence` (선택) — toolspec + toolprofile + security 동시 attach
- `TestIndex_FallbackOnError` — atomic write 실패 시 기존 index 보존

---

## 7. NodeSentinel 관련 forward-reference

v0.6.1 §1과 companion `NodeSentinel_Validation_Data_Plane_설계_v0.1.md`에 따라, **L3~L5의 수행 주체는 host-run NodeVault에 고정하지 않는다**. 향후 K8s data-plane 앱 NodeSentinel로 분리 가능하다.

본 v0.3 draft는 그 분리에 비독립적이다 — 즉, 다음을 만족한다.

- 신규 4개 additive field와 referrer 분리는 NodeVault가 단독 수행해도, NodeSentinel이 수행해도 동일하게 정의된다.
- `validationHash` / `observedProfileDigest` / `securityScanDigest`의 의미는 수행 주체와 독립.
- NodeVault `pkg/validate`는 transition 동안 그대로 유지된다 (사용자 결정 #3).
- NodeSentinel ingress proto · enqueue stub은 본 Sprint 0 범위 밖.

---

## 8. 본 Sprint 0에서 하지 않는 것

- proto 파일 수정 (`nodevault.proto`에 신규 필드 추가)
- `pkg/index/schema.go` 변경
- `pkg/catalog` JSON tag 추가
- 실제 dry-run profiling 구현
- legacy Dockerfile parser 추가
- NodeSentinel ingress stub 추가

위 항목은 Sprint 1 이후 별도 PR/commit으로 진행한다. v0.6.1 §15 Codex Prompt의 strict additive coding rules 준수.

---

## 9. 완료 기준 체크리스트 (Sprint 0)

- [x] 본 문서가 `docs/TOOL_CONTRACT_V0_3_DRAFT.md`로 존재
- [x] 4 신규 optional field 명세 (`authoring_hash` / `validation_hash` / `observed_profile_digest` / `security_scan_digest`)
- [x] 기존 `casHash` 계산 방식 / catalog 경로 / PortSpec field 변경 금지 명시
- [x] 신규 referrer artifactType 3종(toolspec 유지, toolprofile/security 신규) 명시
- [x] NodeSentinel forward-reference (분리 가능성 명시, dependency 없음)
- [x] 본 Sprint 0에서 코드 변경 없음을 명시
- [x] 형제 문서 5개 작성 (OBSERVED_PROFILE_SPEC.md / SECURITY_SCAN_SPEC.md / RUNNER_NODE_SPEC.md / NODEVAULT_V03_MAPPING.md / TOOL_NODE_SPEC.md Layer 5 update)
- [x] `NODEVAULT_TRANSITION_PLAN.md`에 Sprint 0 항목 추가

---

## 10. 형제 문서

- `OBSERVED_PROFILE_SPEC.md` — `toolprofile` referrer payload + `validationHash` 운영 규칙
- `SECURITY_SCAN_SPEC.md` — `security` referrer payload + trivy-operator 통합 + record_only 정책
- `RUNNER_NODE_SPEC.md` — DagEdit RunnerNode contract (`casHash` required, optional metadata)
- `NODEVAULT_V03_MAPPING.md` — v0.6.1 vocabulary ↔ NodeVault 기존 vocabulary + 코드 위치 포인터
- `TOOL_NODE_SPEC.md` Layer 5 — RUNNER_NODE_SPEC.md 참조 추가
- companion: `batch-integration/docs/master-plan/NodeKit/NodeSentinel_Validation_Data_Plane_설계_v0.1.md`
