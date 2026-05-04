# 툴 노드 관련 YAML/JSON 현황

작성일: 2026-04-18  
갱신일: 2026-05-03 (Layer 4 TODO-07 완료 반영, Layer 5 RUNNER_NODE_SPEC.md 추가)  
상태: 현황 기록 (구현 완료/미완료 포함)

이 문서는 플랫폼에서 "툴 노드"가 어떤 형태의 YAML/JSON/spec으로 표현되는지
5개 계층으로 분리해 현재 상태를 기록한다.

---

## 계층 구조 요약

```
[1] 빌드 레시피        Dockerfile + conda spec (사용자 작성)
         ↓ L2 빌드 (podbridge5)
[2] 배포 인프라 YAML   NodeVault K8s Deployment/ConfigMap/Service
         ↓ gRPC BuildRequest
[3] ToolDefinition     NodeKit 작성 → NodeVault 수신 → CAS JSON 저장
         ↓ TODO-07 (구현 완료 — 2026-04-19)
[4] OCI referrer spec  Harbor에 첨부되는 spec artifact ← toolspec 구현 완료 / toolprofile·security 미구현
         ↓ RUNNER_NODE_SPEC.md (설계 완료, DagEdit 구현은 P5)
[5] 파이프라인 노드    DagEdit RunnerNode 안에 casHash 기록 ← 미구현 (P5 이후)
```

---

## 계층 1 — 빌드 레시피 (Dockerfile + conda spec)

### 역할

사용자(관리자)가 NodeKit UI에서 작성하는 빌드 입력이다.
이 내용이 `BuildRequest.dockerfile_content` 필드로 NodeVault에 전송된다.

### 형식

NodeKit이 서버로 전송하는 proto 필드:

```protobuf
message BuildRequest {
  string dockerfile_content = 3;  // Dockerfile 전체 텍스트
  string environment_spec   = 6;  // conda spec (YAML 텍스트)
  ...
}
```

NodeVault가 받아서 podbridge5 in-process 빌드로 전달한다.

### 현재 상태: 동작 중

- `pkg/build/service.go:BuildAndRegister` 가 `req.DockerfileContent` 를 그대로 podbridge5에 전달
- L1 검증(NodeKit): `latest` 태그, digest 미고정, 버전 미고정 패키지 → 차단
- L2(podbridge5): 이미지 빌드 + Harbor push + digest 획득
- L3: K8s dry-run
- L4: smoke run

### 주의

Dockerfile 원본은 빌드 후 **어디에도 보존되지 않는다**.
`BuildRequest` 메시지가 gRPC 스트림으로 전송되고 소모되면 끝이다.
빌드 이력 재현을 위해서는 buildContextRef 또는 sourceDraftRef 저장이 필요하지만
현재 범위에서 의도적으로 제외됐다 (`REGISTERED_TOOL_V0_2_DESIGN.md` 의도적 제외 항목 참조).

---

## 계층 2 — 배포 인프라 YAML (NodeVault K8s 리소스)

### 역할

NodeVault 바이너리가 K8s 위에서 실행되기 위한 Deployment / ConfigMap / Service / GRPCRoute.

### 현재 위치

```
NodeVault/deploy/
├── deployment.yaml     # Deployment (1/1 Running, seoy lab cluster)
├── configmap.yaml      # 환경변수 설정
├── service.yaml        # ClusterIP Service
└── grpcroute.yaml      # Cilium GRPCRoute → nodeforge.10.113.24.96.nip.io:80
```

### 현재 상태: 동작 중

- NodeVault: `nodeforge` namespace, 1/1 Running (2026-04-18 기준)
- gRPC 노출: `nodeforge.10.113.24.96.nip.io:80` (Cilium Gateway API)
- Harbor 연결: `harbor.10.113.24.96.nip.io` (Cilium LB VIP, seoy 호스트에서 route 필요)

### 이름 불일치 주의

현재 리소스 이름은 모두 `nodeforge`다.
최종 목표는 `nodevault`로 rename. api-protos 저장소 제거 완료로 **rename 가능 상태**.

---

## 계층 3 — ToolDefinition / CAS JSON (NodeKit ↔ NodeVault 계약)

### 역할

NodeKit에서 관리자가 툴을 정의한 내용을 proto 직렬화해 NodeVault에 전송하고,
NodeVault가 이를 CAS 파일로 저장하는 중간 표현이다.

### 계약 문서

**TOOL_CONTRACT_V0_2.md** 가 확정 계약이다.

### Proto 흐름

```
NodeKit:
  ToolDefinition (C# 모델)
    → BuildRequest (proto)
      → gRPC stream → NodeVault

NodeVault:
  BuildRequest
    → Build (podbridge5)
    → RegisterToolRequest (proto)
      → pkg/catalog (CAS JSON 저장)
      → pkg/index (vault-index.json 기록)
```

### CAS JSON 저장 위치

```
assets/catalog/{casHash}.tooldefinition
```

JSON 키 (TOOL_CONTRACT_V0_2.md 기준):

```json
{
  "tool_name":          "bwa-mem",
  "version":            "0.7.17",
  "stable_ref":         "bwa-mem@0.7.17",
  "image_uri":          "harbor.../library/bwa-mem:latest",
  "digest":             "sha256:...",
  "command":            ["/app/run.sh"],
  "inputs":             [...],
  "outputs":            [...],
  "display":            { "label": "...", "category": "...", "tags": [...] },
  "environment_spec":   "name: bwa\n...",
  "cas_hash":           "sha256:..."
}
```

### 현재 상태: 동작 중

- NodeKit `HttpCatalogClient`: Catalog REST API 호출로 AdminToolList 표시
- NodeVault `pkg/catalogrest`: `GET /api/v1/tools` → `index.Store.ListActive()`
- NodeVault `pkg/index`: 이중 축 상태 관리 (lifecycle_phase + integrity_health), 15개 테스트 통과

### 알려진 미완료

- NodeKit `dotnet build` 276개 경고 존재 (CA1062, HttpCatalogClient.cs 등)
  → CLAUDE.md §8 위반 — 다음 작업에서 수정 필요
- TODO-04 (v0.2 전체 필드 라운드트립 검증) 미완료

---

## 계층 4 — OCI referrer spec JSON (Harbor 첨부 spec artifact)

### 역할

툴 이미지가 Harbor에 push된 후, 해당 이미지의 **OCI referrer**로 spec JSON을 첨부한다.
이 referrer artifact가 존재해야 `integrity_health = Healthy`가 된다.
referrer 없이 이미지만 있으면 `integrity_health = Partial`.

### 설계

```
Harbor:
  library/bwa-mem:latest
    └── [referrer] mediaType: application/vnd.nodevault.toolspec.v1+json
                  subject: library/bwa-mem@sha256:img-digest
                  content: tool spec JSON
```

referrer spec JSON 구조 (NODEVAULT_DESIGN.md / TOOL_CONTRACT_V0_2.md 기반):

```json
{
  "identity":   { "tool": "bwa-mem", "version": "0.7.17", "stableRef": "bwa-mem@0.7.17" },
  "runtime":    { "image": "harbor.../library/bwa-mem:latest", "imageDigest": "sha256:...", "command": ["/app/run.sh"] },
  "ports":      { "inputs": [...], "outputs": [...] },
  "display":    { "label": "BWA-MEM 0.7.17", "category": "Alignment", ... },
  "provenance": { "toolDefinitionId": "...", "imageDigest": "sha256:...", "registeredAt": "..." }
}
```

### 현재 상태: **구현 완료 (TODO-07, 2026-04-19)**

**구현 내용**:
- `pkg/oras/referrer.go:PushToolSpecReferrer` — sori 라이브러리 wrapping으로 Harbor에 toolspec referrer push
- `pkg/index/schema.go:Entry.SpecReferrerDigest` — referrer digest 저장 필드
- `pkg/index/store.go:SetSpecReferrerDigest` — digest 갱신 메서드
- `pkg/build/service.go:BuildAndRegister` — 등록 후 referrer push → `integrity_health = Partial → Healthy` 전이 (non-fatal)
- 의존성: `github.com/seoyhaein/sori v0.0.2` (oras-go wrapper), `oras.land/oras-go/v2 v2.6.0`

**v0.3에서 추가될 referrer** (Sprint 1~):
- `toolprofile`: `application/vnd.nodevault.toolprofile.v1+json` — observed dry-run profile (스펙: `OBSERVED_PROFILE_SPEC.md`)
- `security`: `application/vnd.nodevault.security.v1+json` — CVE scan result (스펙: `SECURITY_SCAN_SPEC.md`)

세 referrer 모두 `pkg/oras/referrer.go`에서 sori 라이브러리를 통해 push한다.

### integrity_health 전이

```
등록 직후:    integrity_health = Partial  (toolspec referrer 없음)
toolspec push 성공: integrity_health = Healthy
toolprofile push 후: Healthy 유지 (ObservedProfileDigest 갱신만)
security push 후:   Healthy 유지 또는 Warning (CVE 정책에 따라)
```

---

## 계층 5 — 파이프라인 노드 JSON (DagEdit RunnerNode)

### 역할

파이프라인 빌더(DagEdit)에서 사용자가 툴을 캔버스에 추가할 때
해당 노드에 어떤 툴의 어떤 revision(`casHash`)을 사용하는지 기록한 것.

**재현성 원칙**: 파이프라인에 저장되는 것은 `stableRef`가 아니라 `casHash`여야 한다.

### 상세 계약: RUNNER_NODE_SPEC.md

계층 5의 전체 계약(RunnerNode JSON 구조, portBindings vs portMetadata, DagEdit→NodePalette 연결 흐름)은
**`RUNNER_NODE_SPEC.md`** 에 정의됐다 (v0.6.1 Sprint 0, 2026-05-03).

핵심 원칙 요약:

| 필드 | 필수 여부 | 역할 |
|------|-----------|------|
| `casHash` | **필수** | 실행 pin (재현성 보장) |
| `stableRef` | 선택 | UI 표시용 snapshot만 |
| `observedProfileDigest` | 선택 | toolprofile referrer digest (UI badge용) |
| `validationHash` | 선택 | dry-run validation hash (UI badge용) |

### 현재 구현 상태

| 항목 | 상태 |
|------|------|
| `RUNNER_NODE_SPEC.md` 계약 문서 | **완료** (2026-05-03) |
| NodePalette REST 응답에 `observedProfileDigest` 포함 | 미구현 (Sprint 1) |
| DagEdit `RunnerNode`에 `casHash` 필드 추가 | **미구현 (P5 이후)** |
| DagEdit → NodePalette API 연결 | **미구현 (P5 이후)** |

### 연결 흐름

```
DagEdit 팔레트에서 도구 선택
  ↓
GET /api/v1/tools?stableRef=bwa@0.7.17  (NodePalette REST)
  ↓
응답의 casHash → RunnerNode.casHash에 기록 (stableRef는 displaySnapshot용)
```

### 비고

DagEdit 연동은 NODEVAULT_TRANSITION_PLAN.md에서 P5 단계다.
NodeKit의 AdminToolList(관리자 전용)와 달리, DagEdit의 palette는 파이프라인 빌더 사용자 대상이며 별도 설계가 필요하다.

---

## 계층별 완료 현황 요약

| 계층 | 내용 | 상태 |
|------|------|------|
| 1. 빌드 레시피 | Dockerfile → podbridge5 빌드 | 동작 중 |
| 2. 배포 인프라 YAML | NodeVault K8s Deployment | 동작 중 (seoy 호스트 바이너리) |
| 3. ToolDefinition / CAS | NodeKit ↔ NodeVault 계약, pkg/catalog + pkg/index | 동작 중 |
| 4. OCI referrer spec | toolspec 구현 완료 (TODO-07, 2026-04-19) / toolprofile·security Sprint 1~ | **toolspec 완료** |
| 5. 파이프라인 노드 | DagEdit RunnerNode casHash 연결 (계약: RUNNER_NODE_SPEC.md) | **미구현 (P5 이후)** |

---

## 다음 작업

### Sprint 1 (코드 추가)
- **toolprofile referrer**: `pkg/oras/referrer.go:PushToolProfileReferrer` 추가
- **additive field**: `pkg/index/schema.go:Entry`에 `AuthoringHash`, `ValidationHash`, `ObservedProfileDigest`, `SecurityScanDigest` 추가
- **proto field**: `nodevault.proto:RegisteredToolDefinition`에 field 19~22 추가
- 스펙: `TOOL_CONTRACT_V0_3_DRAFT.md`, `OBSERVED_PROFILE_SPEC.md`

### Sprint 2 (Validator/Profiler)
- **L5-a**: Validator/Profiler 패키지 — sample data 실행 + toolprofile referrer push

### 병렬 트랙 (Security Scan)
- **L5-b**: `pkg/oras/referrer.go:PushSecurityReferrer` + reconcile loop 연결
- 스펙: `SECURITY_SCAN_SPEC.md`

### P5 이후
- **DagEdit Catalog 연동**: RunnerNode에 casHash 기록, NodePalette REST API 연결
- 계약: `RUNNER_NODE_SPEC.md`
