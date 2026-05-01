# NodeVault/NodeVault 아키텍처 개요

버전: 1.0  
작성일: 2026-04-18  
상태: 현재 구현 기준 (NODEVAULT_DESIGN.md v0.1 + CATALOG_SERVICE_DESIGN.md v0.1 통합/대체)

관련 문서:
- [NODEVAULT_TRANSITION_PLAN.md](NODEVAULT_TRANSITION_PLAN.md) — TODO 목록 및 구현 계획
- [TOOL_CONTRACT_V0_2.md](TOOL_CONTRACT_V0_2.md) — RegisteredTool v0.2 확정 계약
- [INDEX_SCHEMA.md](INDEX_SCHEMA.md) — index 스키마 및 이중 축 상태 모델
- [AUTHORITY_MAP.md](AUTHORITY_MAP.md) — write authority 분리 설계
- [TOOL_NODE_SPEC.md](TOOL_NODE_SPEC.md) — 툴 노드 YAML/JSON 계층별 현황

---

## 이름 현황 (2026-04-19)

Go 모듈, 바이너리, K8s 리소스, 환경 변수 **모두 NodeVault로 rename 완료**.
로컬 디렉토리(`NodeVault/`)와 GitHub 저장소 이름은 repo rename 후 반영됨.
이 문서는 **NodeVault** 이름으로 통일한다.

---

## 역할 한 줄 정의

플랫폼 아티팩트(툴 이미지, 참조 데이터 이미지)의 생성부터 삭제까지
생명주기 전체를 통제하는 관리 서비스. Harbor 위의 권위 있는 관리자.

---

## 전체 구조 (현재 구현 기준)

```
[NodeKit]  ─── 관리자 UI (C#/Avalonia)
    │ BuildRequest (gRPC :50051)
    │ AdminToolList/AdminDataList (REST :8080 via NodePalette)
    ▼
[NodeVault]  ─── seoy 호스트 바이너리 (100.123.80.48)
    ├── gRPC :50051
    │   ├── BuildService          → L2/L3/L4 + CAS 등록 + index 기록
    │   ├── PolicyService         → DockGuard .wasm 번들 관리
    │   └── ToolRegistryService   → (레거시 gRPC, NodePalette REST로 전환됨)
    │
    └── pkg/catalogrest는 NodePalette 바이너리로 분리됨 (TODO-10 완료)

[NodePalette]  ─── seoy 호스트 바이너리 (100.123.80.48)
    ├── HTTP :8080
    └── pkg/catalogrest       → GET /v1/catalog/tools, /v1/catalog/data
    │
    ├── pkg/build     → podbridge5 in-process 빌드
    ├── pkg/policy    → DockGuard .wasm 번들
    ├── pkg/validate  → K8s dry-run / smoke (L3/L4)
    ├── pkg/catalog   → CAS 파일 저장 (assets/catalog/{casHash})
    ├── pkg/index     → vault-index.json (이중 축 상태 관리)
    └── pkg/reconcile → Harbor 현실 대조 (FastRun/SlowRun)
    │
    ├── Harbor push
    │
    ▼
[Harbor]  ─── harbor.10.113.24.96.nip.io (seoy lab cluster, Cilium LB VIP)
    └── library/<tool>:latest
        └── [spec referrer] ← TODO-07 구현 후 첨부됨 (현재 없음)

[DagEdit]  ─── 파이프라인 빌더 (현재 NodePalette와 연결 없음 — P5 이후)
```

---

## 배포 환경

| 항목 | 값 |
|------|-----|
| 실행 환경 | **seoy 호스트 바이너리** (100.123.80.48) — K8s Pod 아님 |
| 이유 | podbridge5(buildah) rootless 제약 — K8s Pod 안에서 overlay 마운트 불가 |
| gRPC 포트 | `:50051` (seoy 호스트 직접 노출) |
| NodePalette REST 포트 | `:8080` (seoy 호스트 직접 노출, `bin/nodepalette` 별도 바이너리) |
| K8s 접근 | 로컬 kubeconfig (`infra-lab/kubeconfig`) — L3/L4 Job 제출 전용 |
| K8s RBAC | `deploy/02-rbac.yaml` (apply 됨, 미래 in-cluster 전환용으로 미리 배포) |
| 인덱스 저장 | `assets/index/vault-index.json` (seoy 호스트 로컬 파일) |
| Harbor | `harbor.10.113.24.96.nip.io` (Cilium LB VIP 10.113.24.96) |

### NodePalette 위치 (현재 → 목표)

**TODO-10 완료**: `cmd/palette/main.go` → `bin/nodepalette` 별도 바이너리.
NodeVault와 같은 hostvault, assets/ 공유. 요청마다 vault-index.json Reload().
상세 설계: [NODEPALETTE_DESIGN.md](NODEPALETTE_DESIGN.md)

---

## Write Path — 툴 이미지

NodeKit이 `BuildRequest`를 전송하면 NodeVault가 수행하는 순서:

```
1. gRPC 수신 (BuildService.BuildAndRegister)
2. L2: podbridge5로 이미지 빌드 → Harbor push → digest 획득
3. L3: K8s dry-run (Job manifest 검증)
4. L4: K8s smoke run (실제 컨테이너 실행 확인)
5. pkg/catalog: CAS JSON 저장 ({casHash}.tooldefinition)
6. pkg/index: vault-index.json에 entry append (lifecycle_phase=Active)
7. [TODO-07] pkg/oras: spec referrer push → Harbor (현재 미구현, SpecReferrerDigest = empty)
8. 완료 이벤트 스트림 → NodeKit
```

---

## Read Path — Catalog

NodeKit AdminToolList 또는 미래의 파이프라인 빌더 palette:

```
GET /api/v1/tools
  → pkg/catalogrest
    → index.Store.ListActive()
      → lifecycle_phase = Active 항목만 반환
      (integrity_health는 노출 기준에 영향 없음)
```

---

## Artifact 상태 이중 축

index의 상태는 두 축으로 분리한다. **절대 같은 필드에 섞지 않는다.**

| 축 | 값 | 변경 주체 | 의미 |
|----|-----|-----------|------|
| `lifecycle_phase` | Pending / Active / Retracted / Deleted | NodeVault 명시적 호출 | 관리자의 승인 의도 |
| `integrity_health` | Healthy / Partial / Missing / Unreachable / Orphaned | reconcile loop | Harbor 현실과의 대조 결과 |

**NodePalette 노출 규칙**: `lifecycle_phase = Active`만. `integrity_health`는 알람/모니터링 전용.

세부 규칙과 교차 상태 표는 **[INDEX_SCHEMA.md](INDEX_SCHEMA.md)** 참조.

---

## OCI Artifact 명세

### 툴 이미지 referrer (TODO-07 이후)

| 항목 | 값 |
|------|-----|
| mediaType | `application/vnd.nodevault.toolspec.v1+json` |
| subject | 툴 이미지 manifest descriptor |
| content | tool spec JSON (TOOL_CONTRACT_V0_2.md 기준) |

### 참조 데이터 이미지 referrer (P3 이후)

| 항목 | 값 |
|------|-----|
| mediaType | `application/vnd.nodevault.dataspec.v1+json` |
| subject | 데이터 이미지 manifest descriptor |
| content | data spec JSON |

---

## NodeKit 연동

| 요청 | 현재 구현 |
|------|-----------|
| 툴 이미지 빌드 요청 | `BuildService.BuildAndRegister` (gRPC) |
| 툴 목록 조회 (AdminToolList) | `GET /v1/catalog/tools` (NodePalette REST) via `HttpCatalogClient` |
| 데이터 목록 조회 (AdminDataList) | `GET /v1/catalog/data` (NodePalette REST) — 현재 빈 목록 |
| 데이터 등록 요청 | 미구현 (P3 TODO-12) |
| 삭제 / Retract | 미구현 (P4 TODO-14) |

---

## 패키지별 역할 (현재)

| 패키지 | 역할 | 상태 |
|--------|------|------|
| `pkg/build` | Tool Write Path (podbridge5 빌드, L3/L4) | 운영 중 |
| `pkg/policy` | PolicyService (DockGuard .wasm) | 운영 중 |
| `pkg/validate` | K8s dry-run / smoke | 운영 중 |
| `pkg/catalog` | CAS 파일 저장 | 운영 중, pkg/index로 전환 예정 |
| `pkg/index` | 인덱스 관리 (이중 축 상태) | 구현 완료 (15개 테스트) |
| `pkg/catalogrest` | NodePalette REST API (read-only, 향후 `cmd/palette/`로 분리) | 운영 중 |
| `pkg/reconcile` | Harbor 현실 대조 (FastRun/SlowRun) | 구현 완료 (11개 테스트) |
| `pkg/registry` | Harbor digest 조회 (`GetDigest`) | 운영 중 |

---

## 현재 미구현 항목

| 항목 | TODO | 우선순위 |
|------|------|---------|
| OCI spec referrer push (sori 통합) | TODO-07 | P1 (즉시 시작 가능) |
| NodePalette 별도 바이너리 분리 | TODO-10 | P2 (TODO-09b 이후) |
| Retract / Delete lifecycle | TODO-14 | P4 |
| Data Write Path (DataRegisterRequest) | TODO-12 | P3 |
| DagEdit NodePalette 연동 | — | P5 이후 |

---

## 구 설계 문서와의 차이

| 항목 | NODEVAULT_DESIGN.md v0.1 / CATALOG_SERVICE_DESIGN.md v0.1 | 현재 구현 |
|------|-----------------------------------------------------------|-----------|
| NodePalette 위치 | 별도 K8s Deployment | NodeVault 바이너리 내 pkg/catalogrest (TODO-10에서 분리) |
| NodePalette 데이터 소스 | NodeVault 내부 REST API 경유 | 같은 프로세스 index.Store 직접 접근 |
| 인덱스 저장 | bbolt 또는 SQLite (미정) | JSON 파일 (vault-index.json) |
| 실행 환경 | bare metal systemd | K8s Deployment (privileged) |
| Harbor | 미설치 | 운영 중 (harbor.10.113.24.96.nip.io) |
| pkg/index | 신규 예정 | 구현 완료 |
| pkg/oras | 신규 예정 | 스텁만 |
| NodeKit AdminToolList | gRPC ListTools (전환 예정) | Catalog REST (전환 완료) |
