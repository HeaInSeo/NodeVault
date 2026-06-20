# NodeVault <!-- v0.3.1 -->

외부 authoring tool에서 요청을 받아 tool 이미지를 빌드·검증·인증·등록하는 Kubernetes 데이터플레인 애플리케이션.
NodeVault는 장기 실행 Pod로 배포되며, 같은 Pod 안에서 podbridge5가 Buildah Go API를 사용해 이미지를 빌드하고 Harbor에 push한다.
현재 infra-lab 기준 production 배포는 Kubernetes User Namespace(`hostUsers:false`)와 `crun` 기반의 non-privileged Buildah 경로를 사용한다.

→ 전체 플랫폼 구성 및 end-to-end 흐름: [docs/PLATFORM_MAP.md](docs/PLATFORM_MAP.md)
→ TrueNAS/iLO/NFS 운영 메모: [docs/TRUENAS_NFS_RUNBOOK.md](docs/TRUENAS_NFS_RUNBOOK.md)

---

## 전체 구조

```
NodeKit (외부 C# authoring/admin tool)
    │  BuildRequest (gRPC)
    ▼
NodeVault (이 프로젝트 — K8s data-plane Go gRPC + REST app)
    │
    ├── L2: in-pod-buildah (production)
    │       NodeVault process → podbridge5 wrapper → Buildah Go API
    │       hostUsers:false Pod 안에서 container-root를 host 비특권 UID로 매핑
    │       별도 builder Job/worker Pod를 생성하지 않음
    │       disabled → 빌드 없이 gRPC 서버만 기동
    ├── 등록: CAS 저장 + pkg/index append
    ├── NodeSentinel enqueue: pkg/sentinelclient → EnqueueValidationWork gRPC
    ├── Catalog REST: pkg/catalogrest (lifecycle_phase=Active 목록)
    ├── Validation REST: POST /v1/validation/check-records, scan-records (L5 수신)
    └── /debug/vars: expvar 메트릭 (NODEVAULT_METRICS_ADDR, 기본 :9090)
    │
    ▼
Harbor (harbor.lab.local)
    └── library/<tool>@sha256:<digest>

    ▼ EnqueueValidationWork (gRPC)
NodeSentinel (별도 서비스)
    ├── L3: K8s Job dry-run (manifest 검증)
    ├── L4: K8s smoke run (컨테이너 기동 확인)
    ├── L5-a: functional validation Job → validationHash → POST /v1/validation/check-records
    └── L5-b: trivy-operator VulnerabilityReport → POST /v1/validation/scan-records

    ▼ certification.Service (NodeVault 내부)
CertifiedToolImageRecord + ToolFunctionCatalogEntry 생성
→ GET /v1/catalog/certified-tools (NodePalette 조회)
```

---

## gRPC 서비스 목록

| 서비스 | 패키지 | 설명 |
|--------|--------|------|
| `PingService` | `pkg/ping` | 연결 확인 |
| `PolicyService` | `pkg/policy` | DockGuard `.wasm` 번들 제공 (NodeKit L1 정책 평가) |
| `BuildService` | `pkg/build` | L2 빌드 + 등록 + NodeSentinel enqueue; BuildEvent 스트림 |
| `ValidateService` | `pkg/validate` | L3 dry-run / L4 smoke run (BuildService 내부 호출) |
| `ToolRegistryService` | `pkg/catalog` | ToolDefinition CAS 저장 (gRPC write path) |
| `DataRegistryService` | `pkg/catalog` | DataDefinition CAS 저장 (gRPC write path) |
| `ValidationResultService` | `pkg/validation` | L5-a ToolCheckRecord / L5-b ToolScanRecord 수신 + 인증 평가 |

프로토 정의: [`protos/nodevault/v1`](protos/nodevault/v1/)

---

## REST API (`:8082`)

`pkg/catalogrest`가 동일 바이너리 안에서 HTTP REST를 제공한다.

### Catalog (읽기)

| 엔드포인트 | 설명 |
|------------|------|
| `GET /api/v1/tools` | `lifecycle_phase=Active` tool 목록 |
| `GET /api/v1/tools/{casHash}` | casHash 기준 단건 조회 |
| `GET /api/v1/data` | `lifecycle_phase=Active` data 목록 |
| `GET /v1/catalog/certified-tools` | 인증된 tool 팔레트 (PromotionStatus=active) |
| `GET /v1/catalog/certified-tools/{casHash}` | 인증 tool 단건 조회 |

### Validation (NodeSentinel → NodeVault)

| 엔드포인트 | 설명 |
|------------|------|
| `POST /v1/validation/check-records` | L5-a ToolCheckRecord 수신 → certification 평가 |
| `POST /v1/validation/scan-records` | L5-b ToolScanRecord 수신 → certification 재평가 |

### Webhook

| 엔드포인트 | 설명 |
|------------|------|
| `POST /harbor/events` | Harbor push 이벤트 수신 → reconcile 트리거 |
| `GET /healthz` | 헬스체크 → `200 ok` |

---

## 메트릭 (`/debug/vars`)

NodeVault는 `expvar` 기반 메트릭을 `NODEVAULT_METRICS_ADDR`(기본 `:9090`)에서 제공한다.

```bash
curl http://localhost:9090/debug/vars
```

| 카운터 | 설명 |
|--------|------|
| `nodevault_reconcile_fast_total` | FastRun 실행 횟수 (integrity 존재 확인 루프) |
| `nodevault_reconcile_slow_total` | SlowRun 실행 횟수 (pull 도달 가능성 확인 루프) |
| `nodevault_reconcile_error_total` | 리콘사일 루프 오류 횟수 |
| `nodevault_build_success_total` | L2 이미지 빌드 성공 횟수 |
| `nodevault_build_failure_total` | L2 이미지 빌드 실패 횟수 |

---

## 빠른 시작

### 사전 조건

| 도구 | 용도 |
|------|------|
| Go 1.25.11 | 빌드 |
| CGO 빌드 의존성 | `pkg/build` (podbridge5): gpgme, btrfs-progs-devel 등 |
| kubectl | L3/L4 K8s 연동 |
| K8s user namespaces | infra-lab 기준 K8s 1.36.x, Linux 6.8, containerd 2.2, crun 1.28 검증 |

### 빌드 및 실행

```bash
# vendor와 NodeVault image 생성/push
make vendor
make push-image

# Kubernetes 데이터플레인 앱 배포
make deploy-infralab

# 배포된 Service를 port-forward하여 in-pod-buildah 통합 테스트
make test-integration-infralab
```

로컬 바이너리 실행은 디버깅 또는 `disabled` 모드 확인용 compatibility 경로다.

### Buildah / User Namespace 운영 기준

- production backend는 `NODEVAULT_BUILD_BACKEND=in-pod-buildah`다.
- `NODEVAULT_BUILD_BACKEND=k8s-job`은 제거된 spike 경로이며, 현재 바이너리는 이 값을 거부한다.
- NodeVault Pod는 `hostUsers:false`, `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation:false`, `privileged:false` 기준으로 배포한다.
- Buildah는 `BUILDAH_ISOLATION=chroot`, `BUILDAH_RUNTIME=crun` 조합을 사용한다.
- 현재 lab에서는 storage driver를 `overlay`로 둔다. `hostUsers:false`와 함께 동작하도록 Pod AppArmor를 `Unconfined`로 설정하고, podbridge5 chroot-isolation 빌드 경로에 필요한 capability set을 명시한다.
- Harbor는 Gateway hostname인 `harbor.lab.local`을 기준으로 사용한다. infra-lab CoreDNS가 `harbor.lab.local -> 10.113.24.96`을 제공한다.

### Build state 저장소와 향후 DB 전환

Phase 2부터 `pkg/buildstate`는 `SubmitToolBuild`/`WatchToolBuild`/`CancelToolBuild` 경로의 durable execution state를 담당한다. 현재 구현체는 SQLite WAL 기반 로컬 store이며, 목적은 Pod 재시작 후 `Requested`/`Resolving`/`Building`/`Pushing` 상태를 `Interrupted`로 복구하는 것이다.

SQLite 파일 포맷은 플랫폼 외부 계약이 아니다. 장기적으로 NodeVault replica 확장, NodeKit/NodeSentinel/NodePalette의 공통 provenance 조회, build/certification/scan/validation history 통합, 운영 SQL 리포팅이 필요해지면 동일한 `buildstate` 경계를 유지하고 Postgres 같은 통합 DB 구현체로 교체한다. 그 전까지 SQLite는 단일 NodeVault Pod와 PVC 기반 배포에서 운영 상태를 보존하는 초기 production 구현으로 둔다.

### 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `NODEVAULT_ADDR` | `:50051` | gRPC 서버 바인딩 주소 |
| `NODEVAULT_WEBHOOK_ADDR` | `:8082` | Catalog/Validation REST + webhook 수신 주소 |
| `NODEVAULT_METRICS_ADDR` | `:9090` | expvar 메트릭 HTTP 서버 주소 |
| `NODEVAULT_BUILD_BACKEND` | `in-pod-buildah` | 빌드 모드: `in-pod-buildah` / `disabled` (`local-podbridge`는 deprecated alias, `k8s-job`은 제거됨) |
| `NODEVAULT_RUNTIME_MODE` | `host` | Kubernetes 배포에서는 `incluster`; 로컬 compatibility 실행은 `host` |
| `NODEVAULT_FAST_RECONCILE` | `5m` | FastRun 주기 (integrity 존재 확인) |
| `NODEVAULT_SLOW_RECONCILE` | `30m` | SlowRun 주기 (pull 도달 가능성) |
| `NODEVAULT_BUILD_STATE_DB` | `assets/buildstate/build-state.db` | 비동기 빌드 상태 SQLite DB (K8s 배포값: `/data/build-state.db`) |
| `NODEVAULT_REGISTRY_ADDR` | `harbor.lab.local` | 이미지 push 대상 Harbor Gateway hostname; infra-lab CA와 HTTPRoute가 이 hostname을 기준으로 구성됨 |
| `NODEVAULT_ORAS_INSECURE_TLS` | `false` | ORAS referrer push TLS 검증 비활성화 |
| `NODESENTINEL_GRPC_ADDR` | `nodesentinel.nodevault-system.svc.cluster.local:50052` | NodeSentinel EnqueueValidationWork 엔드포인트 |
| `HARBOR_USER` / `HARBOR_PASS` | — | ORAS referrer 호환용 Harbor 인증 정보; Buildah push는 mounted auth.json 사용 |
| `CATALOG_DIR` | `assets/catalog` | tool CAS 파일 저장 디렉토리 |
| `DATA_CATALOG_DIR` | `assets/data-catalog` | data CAS 파일 저장 디렉토리 |
| `INDEX_DIR` | `assets/index` | vault-index.json 저장 디렉토리 |
| `KUBECONFIG` | `~/.kube/config` | 로컬 compatibility/테스트 경로에서만 사용; Pod는 ServiceAccount 사용 |

---

## 테스트

```bash
# 유닛 테스트 (race detector + coverage)
make test

# Proto API lint (Buf)
make buf-lint

# 커버리지 리포트
make coverage

# kube-slint gate (churn 억제 + 회귀 검증)
# 사전 조건: make build
make slint

# 취약점 스캔
make vuln
```

### CI (GitHub Actions)

7개 job 구성 (`[self-hosted, linux, podbridge5]`):

| Job | 내용 |
|-----|------|
| `proto-lint` | Buf `STANDARD` lint (`protos/`) |
| `lint` | golangci-lint (zero-warning) |
| `build` | go build + go vet |
| `test-unit` | -race -cover, coverage artifact 업로드 |
| `vuln-scan` | govulncheck (continue-on-error) |
| `slint-gate` | kube-slint churn/regression gate (25s window) |
| `kube-lint` | kube-linter K8s manifest guardrail |

### kube-slint gate

[kube-slint](https://github.com/HeaInSeo/kube-slint) SLI 게이트는 `NODEVAULT_BUILD_BACKEND=disabled` + `NODEVAULT_FAST_RECONCILE=5s` 조건에서 25초 관찰 창을 측정한다.

**정책** (`.slint/policy.yaml`):

| SLI | 조건 | 목적 |
|-----|------|------|
| `reconcile_fast_delta` | `>= 1` | 리콘사일 루프 liveness |
| `reconcile_fast_delta` | `<= 15` | churn 억제 (25s에 최대 15회) |
| `reconcile_error_delta` | `== 0` | 회귀 없음 |
| `build_failure_delta` | `== 0` | 빌드 실패 없음 |

---

## 패키지 구조

```
cmd/controlplane/     — gRPC + REST 서버 진입점 (main.go)
pkg/
  build/              — BuildService: in-Pod podbridge5/Buildah 빌드 + 등록
  catalog/            — ToolRegistryService / DataRegistryService: CAS 저장/조회
  catalogrest/        — Catalog REST API + Validation REST + Harbor webhook
  certification/      — CertifiedToolImageRecord + ToolFunctionCatalogEntry 생성
  index/              — vault-index.json schema v3: lifecycle, integrity, check/scan/cert 레코드
  metrics/            — expvar 카운터 + /debug/vars HTTP 서버
  oras/               — OCI spec referrer push (sori wrapping)
  ping/               — PingService: 헬스체크
  policy/             — PolicyService: DockGuard .wasm 번들 제공
  reconcile/          — Harbor 현실 대조 → integrity_health 갱신 (FastRun / SlowRun)
  registry/           — OCI 레지스트리 클라이언트
  sentinelclient/     — NodeSentinel EnqueueValidationWork gRPC 클라이언트
  validate/           — ValidateService: L3 dry-run / L4 smoke run
  validation/         — ValidationResultService: L5-a/b 결과 수신 + certification 트리거
test/
  slint/              — kube-slint gate 테스트 (build tag: slint)
protos/nodevault/v1/  — NodeVault gRPC proto 정의
protos/nodesentinel/v1/ — NodeSentinel IngressService proto (EnqueueValidationWork)
deploy/               — K8s 매니페스트 (namespaces, RBAC, Deployment)
.slint/               — kube-slint policy.yaml (슬린트 게이트 임계값)
```

---

## artifact 상태 모델 (Index schema v3)

`vault-index.json`은 세 계층으로 artifact 상태를 추적한다.

### 이중 축 (lifecycle / integrity)

| 축 | 값 | 변경 주체 | 의미 |
|----|-----|-----------|------|
| `lifecycle_phase` | Pending / Active / Retracted / Deleted | NodeVault 명시적 호출만 | 관리자의 승인 의도 |
| `integrity_health` | Healthy / Partial / Missing / Unreachable / Orphaned | reconcile loop만 | Harbor 현실 대조 결과 |

### 검증·인증 레코드 (v3 추가)

| 레코드 | 생성 주체 | 설명 |
|--------|-----------|------|
| `ToolCheckRecord` | NodeSentinel L5-a | functional validation 결과 (validationHash 포함) |
| `ToolScanRecord` | NodeSentinel L5-b | trivy-operator 취약성 스캔 결과 |
| `CertifiedToolImageRecord` | NodeVault `certification.Service` | check+scan 평가 후 인증 결정 (PromotionStatus) |
| `ToolFunctionCatalogEntry` | NodeVault `certification.Service` | NodePalette 노출 항목 (PromotionStatus=active만 표시) |

**Catalog 노출**: `lifecycle_phase = Active`인 것만. **팔레트 노출**: `PromotionStatus = active`인 것만.

---

## 오케스트레이션 흐름

빌드 요청부터 팔레트 노출까지 전체 순서:

```
1. L2: BuildService — 빌드 백엔드 → Harbor push → digest 확보
2. 등록:
   ├── pkg/catalog: CAS JSON 저장
   ├── pkg/index: vault-index.json append (lifecycle_phase=Active)
   └── pkg/oras: OCI spec referrer push → integrity_health=Healthy
3. pkg/sentinelclient: NodeSentinel EnqueueValidationWork 호출

[NodeSentinel이 비동기 실행]
4. L3: K8s Job dry-run — manifest admission 검증
5. L4: K8s smoke run — 컨테이너 기동 + 정상 종료 확인
6. L5-a: functional validation Job → validationHash 계산
   → POST /v1/validation/check-records (NodeVault REST)
7. L5-b: trivy-operator VulnerabilityReport 조회
   → POST /v1/validation/scan-records (NodeVault REST)

[NodeVault certification.Service 평가]
8. ToolCheckRecord / ToolScanRecord → CertifiedToolImageRecord + ToolFunctionCatalogEntry
9. GET /v1/catalog/certified-tools → NodePalette → DagEdit 팔레트 노출
```

이벤트는 `BuildEvent` 스트림으로 NodeKit에 실시간 전달 (L2까지).

---

## DockGuard 정책

NodeKit L1 정책 평가에 사용되는 `.wasm` 번들은 [`DockGuard`](https://github.com/HeaInSeo/DockGuard) 레포에서 빌드.

| 패키지 | 규칙 | 설명 |
|--------|------|------|
| `dockerfile.multistage` | DFM001–DFM004 | 멀티스테이지 빌드 강제 |
| `dockerfile.security` | DSF001–DSF003 | root 실행 금지, 시크릿 노출 금지, ADD URL 금지 |
| `dockerfile.genomics` | DGF001–DGF002 | conda/pip 버전 고정 강제 |

---

## 관련 프로젝트

| 프로젝트 | 역할 |
|----------|------|
| [`NodeKit`](https://github.com/HeaInSeo/NodeKit) | C# 어드민 UI — ToolDefinition 편집, L1 검증, BuildRequest gRPC 전송 |
| [`NodeSentinel`](https://github.com/HeaInSeo/NodeSentinel) | K8s data-plane 검증 에이전트 — L3/L4/L5-a/L5-b Job 실행 |
| [`NodePalette`](https://github.com/HeaInSeo/NodePalette) | 인증 tool 팔레트 REST 서비스 — GET /v1/palette/tools (DagEdit 등 소비) |
| [`DockGuard`](https://github.com/HeaInSeo/DockGuard) | OPA/Rego Dockerfile 정책 + .wasm 번들 빌드 |
| [`kube-slint`](https://github.com/HeaInSeo/kube-slint) | K8s SLI 게이트 (churn 억제, 회귀 검증) |
| [`bori`](https://github.com/HeaInSeo/bori) | K8s 오퍼레이터 — BoriDataPlane CRD로 NodeVault 배포 관리 (예정) |
| [`infra-lab`](https://github.com/HeaInSeo/infra-lab) | VM 기반 K8s 테스트 클러스터 (multipass / libvirt backend) |
