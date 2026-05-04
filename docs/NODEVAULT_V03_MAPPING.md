# NodeVault v0.3 Mapping — 설계 용어 ↔ 코드 위치

버전: v0.1.0
작성일: 2026-05-03
상태: Sprint 0 유지 문서 (코드 추가 시 갱신)
근거: `NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` + 실제 코드 대조

이 문서는 v0.6.1 설계 용어와 NodeVault 실제 코드/파일 위치를 대응시킨다.
새 기능 구현 전 "이게 코드 어디에 있나"를 빠르게 찾는 용도.

---

## 1. 핵심 식별자

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| `casHash` | `pkg/index/schema.go:Entry.CasHash` | 구현 완료 |
| `casHash` 계산 | `pkg/catalog/catalog.go` (`SHA256(spec JSON without cas_hash)`) | 구현 완료 |
| `casHash` catalog 경로 | `assets/catalog/{casHash}.tooldefinition` | 구현 완료 |
| `stableRef` | `pkg/index/schema.go:Entry.StableRef` (`tool_name@version`) | 구현 완료 |
| `stableRef` 기준 조회 | `pkg/index/store.go:ListByStableRef` | 구현 완료 |
| `authoringHash` | `pkg/index/schema.go` (미추가) | **미구현 — Sprint 1** |
| `validationHash` | `pkg/index/schema.go` (미추가) | **미구현 — Sprint 1** |
| `observedProfileDigest` | `pkg/index/schema.go` (미추가) | **미구현 — Sprint 1** |
| `securityScanDigest` | `pkg/index/schema.go` (미추가) | **미구현 — 병렬 트랙** |

---

## 2. 상태 이중 축

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| `lifecycle_phase` | `pkg/index/schema.go:LifecyclePhase` | 구현 완료 |
| `lifecycle_phase` 값 | `PhasePending/PhaseActive/PhaseRetracted/PhaseDeleted` | 구현 완료 |
| `lifecycle_phase` 변경 | `pkg/catalog/catalog.go:RetractTool`, `DeleteTool` + `pkg/index/store.go:SetLifecyclePhase` | 구현 완료 |
| `integrity_health` | `pkg/index/schema.go:IntegrityHealth` | 구현 완료 |
| `integrity_health` 값 | `HealthHealthy/Partial/Missing/Unreachable/Orphaned` | 구현 완료 |
| `integrity_health` 변경 | `pkg/reconcile/reconciler.go` (reconcile loop 전용) | 구현 완료 |
| Catalog 노출 기준 | `pkg/index/store.go:ListActive` (`lifecycle_phase = Active`만) | 구현 완료 |

**불변 규칙**: `lifecycle_phase`는 NodeVault 명시적 호출만. `integrity_health`는 reconcile loop만. 교차 금지.

---

## 3. OCI Referrer

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| `toolspec` referrer push | `pkg/oras/referrer.go:PushToolSpecReferrer` | 구현 완료 (TODO-07) |
| `toolspec` artifactType | `application/vnd.nodevault.toolspec.v1+json` | 구현 완료 |
| `toolspec` digest 저장 | `pkg/index/schema.go:Entry.SpecReferrerDigest` + `pkg/index/store.go:SetSpecReferrerDigest` | 구현 완료 |
| `toolprofile` referrer push | `pkg/oras/referrer.go` (`PushToolProfileReferrer` 미구현) | **미구현 — Sprint 1** |
| `toolprofile` artifactType | `application/vnd.nodevault.toolprofile.v1+json` | **미구현** |
| `toolprofile` digest 저장 | `pkg/index/schema.go:Entry.ObservedProfileDigest` (미추가) | **미구현 — Sprint 1** |
| `security` referrer push | `pkg/oras/referrer.go` (`PushSecurityReferrer` 미구현) | **미구현 — 병렬 트랙** |
| `security` artifactType | `application/vnd.nodevault.security.v1+json` | **미구현** |
| `security` digest 저장 | `pkg/index/schema.go:Entry.SecurityScanDigest` (미추가) | **미구현 — 병렬 트랙** |
| OCI referrer 라이브러리 | `github.com/seoyhaein/sori v0.0.2` + `oras.land/oras-go/v2 v2.6.0` | 구현 완료 |

---

## 4. 검증 계층 (L1~L5)

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| L1 (DockGuard WASM) | NodeKit — `WasmPolicyChecker` | NodeKit 소유 |
| L2 (image build) | `pkg/build/service.go` (podbridge5 in-process) | 구현 완료 |
| L3 (K8s dry-run) | `pkg/validate/service.go:DryRunJob` | 구현 완료 |
| L4 (smoke run) | `pkg/validate/service.go:SmokeRunJob` (timeout: 5분) | 구현 완료 |
| L5-a (Validator/Profiler) | `pkg/validate/` (미구현) | **미구현 — Sprint 2** |
| L5-b (Security Scan) | `pkg/reconcile/` + trivy-operator (미구현) | **미구현 — 병렬 트랙** |
| dry-run namespace | `nodevault-builds` | 구현 완료 |
| smoke run namespace | `nodevault-smoke` (상수: `pkg/validate/service.go:smokeNamespace`) | 구현 완료 |
| smoke run timeout | `pkg/validate/service.go:smokeTimeout = 5*time.Minute` | 구현 완료 |
| L4 Job spec | `pkg/validate/service.go:SmokeJobSpec` | 구현 완료 |

---

## 5. Catalog / Index / Storage

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| RegisteredToolDefinition (proto) | `protos/nodevault/v1/nodevault.proto:RegisteredToolDefinition` | 구현 완료 |
| RegisteredToolDefinition (CAS JSON) | `pkg/catalog/catalog.go` + `assets/catalog/{casHash}.tooldefinition` | 구현 완료 |
| index 단일 제어 계층 | `pkg/index/store.go` | 구현 완료 |
| index 파일 | `assets/vault-index.json` | 구현 완료 |
| index append | `pkg/index/store.go:Append` | 구현 완료 |
| casHash 기준 역조회 | `pkg/index/store.go:GetByCasHash` | 구현 완료 |
| active 목록 | `pkg/index/store.go:ListActive` | 구현 완료 |
| Catalog 노출 REST | `pkg/catalogrest` + `cmd/palette/main.go` | 구현 완료 |

---

## 6. Reconcile Loop

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| FastRun (빠른 reconcile) | `pkg/reconcile/reconciler.go:FastRun` | 구현 완료 |
| SlowRun (느린 reconcile) | `pkg/reconcile/reconciler.go:SlowRun` | 구현 완료 |
| `integrity_health` 갱신 | `pkg/reconcile/reconciler.go:reconcileExistence`, `reconcileReachability` | 구현 완료 |
| ReconcileOne (단건 즉시 검사) | `pkg/reconcile/reconciler.go:ReconcileOne` | 구현 완료 |
| reconcile 간격 설정 | `NODEVAULT_FAST_RECONCILE` env var (기본 5분, 테스트용 30초) | 구현 완료 |

---

## 7. gRPC 서비스

| v0.6.1 용어 | NodeVault 코드/파일 | 상태 |
|-------------|---------------------|------|
| `ToolRegistryService` | `protos/nodevault/v1/nodevault.proto` + `pkg/catalog/catalog.go` | 구현 완료 |
| `RegisterTool` RPC | `pkg/catalog/catalog.go:RegisterTool` | 구현 완료 |
| `ListTools` RPC | `pkg/catalog/catalog.go:ListTools` | 구현 완료 |
| `GetTool` RPC | `pkg/catalog/catalog.go:GetTool` | 구현 완료 |
| `RetractTool` RPC | `pkg/catalog/catalog.go:RetractTool` | 구현 완료 |
| `DeleteTool` RPC | `pkg/catalog/catalog.go:DeleteTool` | 구현 완료 |
| `ValidateService` | `protos/nodevault/v1/nodevault.proto` + `pkg/validate/service.go` | 구현 완료 |
| `PolicyService` | `protos/nodevault/v1/nodevault.proto` + `pkg/policy/` | 구현 완료 |
| `BuildService` | `protos/nodevault/v1/nodevault.proto` + `pkg/build/service.go` | 구현 완료 |

---

## 8. 미구현 항목 요약 (Sprint 1+)

### Sprint 1 (코드 추가)

```
proto/nodevault.proto
  + authoring_hash (field 19)
  + validation_hash (field 20)
  + observed_profile_digest (field 21)
  + security_scan_digest (field 22)
  + staging (PortSpec field 8)

pkg/index/schema.go:Entry
  + AuthoringHash          string  `json:"authoring_hash,omitempty"`
  + ValidationHash         string  `json:"validation_hash,omitempty"`
  + ObservedProfileDigest  string  `json:"observed_profile_digest,omitempty"`
  + SecurityScanDigest     string  `json:"security_scan_digest,omitempty"`

pkg/oras/referrer.go
  + PushToolProfileReferrer(...)

pkg/index/store.go
  + SetObservedProfileDigest(...)
```

### Sprint 2 (Validator/Profiler)

```
pkg/validate/ 또는 pkg/profiler/ (신규)
  - sample data 실행
  - observedIoProfile 수집
  - validationHash 계산
  - PushToolProfileReferrer 호출
```

### 병렬 트랙 (Security Scan)

```
pkg/oras/referrer.go
  + PushSecurityReferrer(...)

pkg/reconcile/
  + VulnerabilityReport CR 조회
  + security summary 추출
  + PushSecurityReferrer 호출
  + SetSecurityScanDigest 호출

pkg/index/store.go
  + SetSecurityScanDigest(...)
```

---

## 9. 관련 문서

- `TOOL_CONTRACT_V0_2.md` — v0.2 확정 계약 (변경 금지)
- `TOOL_CONTRACT_V0_3_DRAFT.md` — v0.3 additive field 명세
- `OBSERVED_PROFILE_SPEC.md` — `toolprofile` referrer 스펙
- `SECURITY_SCAN_SPEC.md` — `security` referrer 스펙
- `RUNNER_NODE_SPEC.md` — DagEdit RunnerNode contract
- `AUTHORITY_MAP.md` — write authority 경계
