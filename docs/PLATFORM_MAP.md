# Platform Map

버전: 1.0  
작성일: 2026-04-18  
목적: **개발 세션 시작 시 이 파일 하나로 전체 플랫폼 맥락 파악**

→ 전체 일정/작업 큐: [PLATFORM_SCHEDULE.md](PLATFORM_SCHEDULE.md)

---

## 플랫폼 한 줄 정의

관리자가 생물정보학 툴 이미지를 정의·검증·빌드·등록하고, 파이프라인 빌더가 그 툴을 가져다 실행 파이프라인을 구성하는 플랫폼.

---

## 컴포넌트 지도

```
[관리자]
    │ Tool 정의 + L1 검증
    ▼
[NodeKit]  ── C#/Avalonia 데스크톱 클라이언트
    │ BuildRequest (gRPC)          AdminToolList (REST)
    ▼                                     ▲
[NodeVault]  ── Go 서버                  │
    ├── BuildService     L2→L3→L4 + 등록 │
    ├── PolicyService    DockGuard .wasm  │
    ├── pkg/index        artifact 상태 원장 (이중 축)
    └── pkg/catalogrest  NodePalette REST (현재 인라인, TODO-10에서 분리)
    │ 이미지 push / pull
    ▼
[Harbor]  ── OCI 레지스트리 (harbor.10.113.24.96.nip.io)
    └── library/<tool>:latest
        └── [spec referrer] ← TODO-07 미구현

[DockGuard]  ── OPA/Rego 정책 (.wasm 번들)
    └── NodeKit WasmPolicyChecker가 로컬 실행
    └── NodeVault PolicyService가 번들 배포

[DagEdit]  ── C#/Avalonia 파이프라인 빌더
    └── NodePalette와 연결 없음 ← P5 이후 과제

[sori]  ── 참조 데이터 패키징
    └── 프로토타입 상태, NodeVault P3에서 통합 예정
```

---

## 저장소 경로

| 저장소 | 경로 | 언어 |
|--------|------|------|
| NodeVault | `/opt/go/src/github.com/HeaInSeo/NodeVault` | Go |
| NodeKit | `/opt/dotnet/src/github.com/HeaInSeo/NodeKit` | C# / Avalonia |
| DockGuard | `/opt/dotnet/src/github.com/HeaInSeo/DockGuard` | OPA/Rego |
| DagEdit | `/opt/dotnet/src/github.com/HeaInSeo/DagEdit` | C# / Avalonia |
| sori | `/opt/go/src/github.com/HeaInSeo/sori` | Go |

---

## 인프라 (seoy, 100.123.80.48)

| 항목 | 값 |
|------|-----|
| K8s 클러스터 | multipass VM 3노드 (lab-master-0, lab-worker-0, lab-worker-1) |
| CNI | Cilium |
| Harbor | `harbor.10.113.24.96.nip.io` (Cilium LB VIP 10.113.24.96) |
| NodeVault gRPC | `100.123.80.48:50051` (seoy 호스트 직접, Cilium GRPCRoute는 미사용) |
| NodePalette REST (현재 NodeVault 내 인라인) | `http://100.123.80.48:8080` |
| Harbor admin | `Harbor12345` |
| kubeconfig | `/opt/go/src/github.com/HeaInSeo/infra-lab/kubeconfig` |

seoy 호스트에서 Harbor 접근 시 라우트 필요:
```bash
ip route add 10.113.24.96/32 via 10.113.24.254
```
(multipassd 시작 시 자동 추가됨 — systemd drop-in `discard-ns.conf` ExecStartPost)

---

## end-to-end 흐름 (Tool 빌드 happy path)

```
1. NodeKit AuthoringPanel: ToolDefinition 작성
   ├── L1 정적 검증 (RequiredFields / ImageUri / PackageVersion)
   └── DockGuard .wasm 정책 검사 (WasmPolicyChecker)

2. BuildRequest → gRPC → NodeVault BuildService
   ├── L2: podbridge5 in-process 이미지 빌드 → Harbor push → digest 획득
   ├── L3: K8s Job dry-run
   └── L4: K8s smoke run

3. 등록
   ├── pkg/catalog: CAS JSON 저장 (assets/catalog/{casHash}.tooldefinition)
   ├── pkg/index: vault-index.json append (lifecycle_phase=Active, integrity_health=Partial)
   └── [TODO-07] pkg/oras: spec referrer push → Harbor ← 현재 미구현

4. BuildEvent 스트림 → NodeKit 빌드 로그 표시

5. NodeKit AdminToolList
   └── GET /v1/catalog/tools → NodePalette(pkg/catalogrest) → index.Store.ListActive()
```

---

## artifact 상태 이중 축 (핵심 설계 원칙)

```
lifecycle_phase       integrity_health
─────────────         ────────────────
Pending               Healthy
Active        ✗       Partial          ← 절대 혼합 금지
Retracted     절대     Missing
Deleted       혼합     Unreachable
                      Orphaned
```

| 축 | 변경 주체 | 의미 |
|----|-----------|------|
| `lifecycle_phase` | NodeVault 명시적 호출만 | 관리자의 승인 의도 (NodePalette 노출 기준) |
| `integrity_health` | reconcile loop만 | Harbor 현실 대조 결과 (알람 전용) |

**NodePalette 노출**: `lifecycle_phase = Active`인 것만. `integrity_health`는 무관.

---

## 현재 구현 상태

### 동작 중

| 기능 | 위치 |
|------|------|
| L1 정적 검증 + DockGuard 정책 | NodeKit `src/Validation/`, `src/Policy/` |
| BuildRequest gRPC 전송 | NodeKit `GrpcBuildClient` |
| L2 이미지 빌드 (podbridge5) | NodeVault `pkg/build` |
| L3/L4 K8s dry-run / smoke | NodeVault `pkg/validate` |
| CAS 파일 저장 | NodeVault `pkg/catalog` |
| artifact index (이중 축 상태) | NodeVault `pkg/index` (15개 테스트 통과) |
| NodePalette REST API | NodeVault `pkg/catalogrest` (향후 `cmd/palette/` 분리) |
| AdminToolList REST 연동 | NodeKit `HttpCatalogClient` |
| DockGuard 정책 번들 동적 로드 | NodeKit `GrpcPolicyBundleProvider` |
| Harbor | seoy lab cluster, all components healthy |
| reconcile loop (FastRun / SlowRun) | NodeVault `pkg/reconcile` (11개 테스트 통과) |
| proto canonical source (go.work 없음) | NodeVault `protos/nodeforge/v1/` |

### 미구현 (우선순위 순)

| 항목 | 위치 | TODO | 우선순위 |
|------|------|------|---------|
| OCI spec referrer push (sori 통합) | NodeVault `pkg/build` + sori | TODO-07 | **P1 — 지금 시작 가능** |
| NodeKit compiler warning 276개 | NodeKit CA1062 | — | **즉시 수정** |
| NodePalette 별도 바이너리 분리 | NodeVault `cmd/palette/` | TODO-10 | P2 (TODO-09b 이후) |
| Retract/Delete lifecycle | NodeVault | TODO-14 | P4 |
| Data write path (DataRegisterRequest) | NodeKit + NodeVault | TODO-12 | P3 |
| DagEdit NodePalette 연동 | DagEdit | — | P5 |

---

## 핵심 설계 결정 (변경 시 이 목록 확인)

| 결정 | 내용 |
|------|------|
| 재현성 | `latest` 태그, digest 미고정, 버전 미고정 → L1 차단. bypass 없음 |
| stableRef vs casHash | stableRef = UI 탐색용 / casHash = pipeline pin, 실행 pin |
| stableRef cardinality | 1:N — 동일 stableRef에 여러 casHash 허용, 동시 Active 허용 |
| NodePalette 노출 기준 | lifecycle_phase = Active만. integrity_health는 무관 |
| index write 권한 | NodeVault only (pkg/index.Store를 통해서만) |
| lifecycle_phase 변경 권한 | NodeVault 명시적 호출만 |
| integrity_health 변경 권한 | reconcile loop만 |
| NodePalette 위치 | NodeVault 바이너리 내 pkg/catalogrest (TODO-10에서 cmd/palette/ 분리) |
| NodePalette 이름 결정 | tools + data 양쪽 포함. NODEPALETTE_DESIGN.md 참조 |

---

## 알려진 제약

| 제약 | 내용 |
|------|------|
| NodeKit compiler warning | 276개 (CA1062). CLAUDE.md §8 위반 상태. 다음 작업 전 수정 필요 |
| spec referrer 없음 | 현재 등록된 모든 툴 integrity_health = Partial (TODO-07 미완료) |
| GrpcToolRegistryClient | NodeKit에 존재하나 MainWindow 미사용 레거시 — 향후 삭제 예정 |

---

## 상세 문서 링크

### NodeVault

| 문서 | 위치 | 내용 |
|------|------|------|
| ARCHITECTURE.md | `NodeVault/docs/` | NodeVault 컴포넌트 구조, 실제 구현 기준 |
| NODEVAULT_TRANSITION_PLAN.md | `NodeVault/docs/` | **전체 TODO 목록 + 완료 현황** |
| TOOL_CONTRACT_V0_2.md | `NodeVault/docs/` | RegisteredTool v0.2 확정 계약 |
| INDEX_SCHEMA.md | `NodeVault/docs/` | index 스키마 + 이중 축 상태 모델 |
| AUTHORITY_MAP.md | `NodeVault/docs/` | write authority 분리 설계 |
| NONGOALS.md | `NodeVault/docs/` | 이번 버전에서 의도적으로 하지 않는 것 |
| TOOL_NODE_SPEC.md | `NodeVault/docs/` | 툴 노드 YAML/JSON 5계층 현황 |
| DEPLOY_IN_CLUSTER.md | `NodeVault/docs/` | K8s 배포 절차 |
| PROTO_OWNERSHIP_SPRINT_PLAN.md | `NodeVault/docs/` | api-protos → NodeVault proto 이관 계획 |
| CATALOG_CACHE_STRATEGY.md | `NodeVault/docs/` | Catalog 캐시 전략 |
| SORI_INTEGRATION_BOUNDARY.md | `NodeVault/docs/` | sori 통합 경계 |
| HARBOR_WEBHOOK_EVENTS.md | `NodeVault/docs/` | Harbor webhook 이벤트 목록 |

### NodeKit

| 문서 | 위치 | 내용 |
|------|------|------|
| CLAUDE.md | `NodeKit/` | **책임 경계, 재현성 규칙, 결정 체크리스트 (규범 문서)** |
| ARCHITECTURE.md | `NodeKit/docs/` | NodeKit 컴포넌트 레이어, 외부 연결, 알려진 이슈 |
| NODEKIT_UI_STRUCTURE.md | `NodeKit/docs/` | UI 패널 구조 + L1 검증 흐름 |
| NODEKIT_BUILD_BOOTSTRAP.md | `NodeKit/docs/` | 빌드 환경 설정 (api-protos 경로 등) |
