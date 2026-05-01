# NodeVault

관리자 UI(NodeKit)에서 `BuildRequest`를 받아 tool 이미지를 빌드·검증·등록하는 제어 플레인 서버.
미래의 NodeVault. gRPC 서버 + Catalog REST API를 단일 바이너리로 제공한다.

→ 전체 플랫폼 구성 및 end-to-end 흐름: [docs/PLATFORM_MAP.md](docs/PLATFORM_MAP.md)
→ TrueNAS/iLO/NFS 운영 메모: [docs/TRUENAS_NFS_RUNBOOK.md](docs/TRUENAS_NFS_RUNBOOK.md)

---

## 전체 구조

```
NodeKit (C# 어드민 UI)
    │  BuildRequest (gRPC)
    ▼
NodeVault (이 프로젝트 — Go gRPC + REST 서버)
    │
    ├── L2: podbridge5 in-process 이미지 빌드 → Harbor push
    ├── L3: K8s Job dry-run (스키마 검증)
    ├── L4: K8s smoke run (컨테이너 실행 검증)
    ├── 등록: CAS 저장 + pkg/index append (lifecycle_phase=Active)
    └── Catalog REST: pkg/catalogrest (lifecycle_phase=Active만 노출)
    │
    ▼
Harbor (harbor.10.113.24.96.nip.io)
    └── library/<tool>:latest + digest
```

---

## gRPC 서비스 목록

| 서비스 | 패키지 | 설명 |
|--------|--------|------|
| `PingService` | `pkg/ping` | 연결 확인 |
| `PolicyService` | `pkg/policy` | DockGuard `.wasm` 번들 제공 (NodeKit L1 정책 평가) |
| `BuildService` | `pkg/build` | L2→L3→L4→등록 전체 파이프라인 + BuildEvent 스트림 |
| `ValidateService` | `pkg/validate` | L3 dry-run / L4 smoke run (BuildService 내부 호출) |

프로토 정의: [`protos/nodeforge/v1`](protos/nodeforge/v1/nodeforge.proto)

---

## Catalog REST API

`pkg/catalogrest`가 동일 바이너리 안에서 HTTP REST를 제공한다.

| 엔드포인트 | 설명 |
|------------|------|
| `GET /api/v1/tools` | `lifecycle_phase=Active` tool 목록 |
| `GET /api/v1/data` | `lifecycle_phase=Active` data 목록 |
| `GET /api/v1/tools/{casHash}` | casHash 기준 단건 조회 |

---

## 빠른 시작

### 사전 조건

| 도구 | 용도 |
|------|------|
| Go 1.22+ | 빌드 |
| CGO 빌드 의존성 | `pkg/build` (podbridge5): gpgme, btrfs-progs-devel 등 |
| kubectl | L3/L4 K8s 연동 |

> `pkg/build` 는 CGO 의존성이 있어 개발 호스트에서 `go build ./...` 실패 가능.
> `go test ./pkg/index/... ./pkg/catalog/... ./pkg/catalogrest/...` 등 순수 Go 패키지는 호스트에서 실행 가능.
> 전체 빌드·배포는 Dockerfile 기준 (CGO 의존성 포함).

### 빌드 및 실행

```bash
# Docker 이미지 빌드 (CGO 포함)
make image

# K8s에 배포 (seoy 클러스터)
make deploy

# 로컬 실행 (CGO 의존성 설치된 경우)
./bin/nodevault
```

### 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `NODEVAULT_ADDR` | `:50051` | gRPC 서버 바인딩 주소 |
| `NODEVAULT_REGISTRY_ADDR` | `harbor.10.113.24.96.nip.io` | 이미지 push 대상 Harbor 주소 |
| `NODEVAULT_BUILD_NAMESPACE` | `nodevault-builds` | L4 smoke run Job 실행 네임스페이스 |
| `DOCKGUARD_WASM_PATH` | `assets/policy/dockguard.wasm` | DockGuard 정책 번들 경로 |
| `CATALOG_DIR` | `assets/catalog` | tool CAS 파일 저장 디렉토리 |
| `DATA_CATALOG_DIR` | `assets/data-catalog` | data CAS 파일 저장 디렉토리 |
| `INDEX_DIR` | `assets/index` | vault-index.json 저장 디렉토리 |
| `KUBECONFIG` | `~/.kube/config` | K8s 클러스터 인증 |

---

## 테스트

```bash
# 순수 Go 패키지 (CGO 불필요)
go test ./pkg/index/... ./pkg/catalog/... ./pkg/catalogrest/... \
        ./pkg/reconcile/... ./pkg/validate/... ./pkg/policy/... ./pkg/registry/...

# 또는 (pkg/build·cmd 실패는 CGO 의존성 부재로 정상)
go test ./pkg/...
```

**결과 (CGO-free 패키지 합산)**: 85개 PASS
- pkg/index 15 · pkg/catalog 16 · pkg/catalogrest 14
- pkg/reconcile 11 · pkg/validate 12 · pkg/policy 7 · pkg/registry 10

---

## 패키지 구조

```
cmd/controlplane/     — gRPC + REST 서버 진입점 (main.go)
pkg/
  build/              — BuildService: podbridge5 in-process 이미지 빌드 → Harbor push + digest 확보
  catalog/            — tool/data RegisteredDefinition CAS 저장/조회
  catalogrest/        — Catalog REST API (read-only, lifecycle_phase=Active 필터)
  index/              — vault-index.json 원장: lifecycle_phase / integrity_health 이중 축 상태 관리
  ping/               — PingService: 헬스체크
  policy/             — PolicyService: DockGuard .wasm 번들 제공
  reconcile/          — Harbor 현실 대조 → integrity_health 갱신 (FastRun / SlowRun)
  registry/           — OCI 레지스트리 클라이언트 (GetDigest)
  validate/           — ValidateService: L3 dry-run / L4 smoke run
protos/
  nodeforge/v1/       — canonical gRPC proto source
assets/
  policy/             — dockguard.wasm
  catalog/            — tool CAS 파일
  data-catalog/       — data CAS 파일
  index/              — vault-index.json
deploy/
  00-namespaces.yaml  — nodevault-system / nodevault-builds / nodevault-smoke
  02-rbac.yaml        — ServiceAccount + ClusterRole + ClusterRoleBinding
  03-nodevault.yaml   — NodeVault Deployment
  04-grpcroute.yaml   — Cilium GRPCRoute (nodevault.10.113.24.96.nip.io:80)
docs/
  PLATFORM_MAP.md     — 전체 플랫폼 구성 (시작 시 먼저 읽을 것)
  ARCHITECTURE.md     — NodeVault 컴포넌트 구조 (현재 구현 기준)
```

---

## artifact 상태 이중 축

index(`vault-index.json`)는 두 축으로 artifact 상태를 분리한다.

| 축 | 값 | 변경 주체 | 의미 |
|----|-----|-----------|------|
| `lifecycle_phase` | Pending / Active / Retracted / Deleted | NodeVault 명시적 호출만 | 관리자의 승인 의도 |
| `integrity_health` | Healthy / Partial / Missing / Unreachable / Orphaned | reconcile loop만 | Harbor 현실 대조 결과 |

**Catalog 노출**: `lifecycle_phase = Active`인 것만. `integrity_health`는 무관.

---

## 오케스트레이션 흐름

`BuildService.BuildAndRegister` 스트리밍 RPC 실행 순서:

```
1. L2: podbridge5 in-process 이미지 빌드 → Harbor push → digest 확보
2. L3: K8s dry-run — smoke Job spec 스키마 검증
3. L4: K8s smoke run — 실제 Job 실행, 정상 종료 확인
4. 등록:
   ├── pkg/catalog: CAS JSON 저장
   └── pkg/index: vault-index.json append
       (lifecycle_phase=Active, integrity_health=Partial ← TODO-07 완료 전)
```

이벤트는 `BuildEvent` 스트림으로 NodeKit에 실시간 전달.

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
| [`DockGuard`](https://github.com/HeaInSeo/DockGuard) | OPA/Rego Dockerfile 정책 + .wasm 번들 빌드 |
| `protos/` | NodeVault canonical gRPC proto source |
| [`infra-lab`](https://github.com/HeaInSeo/infra-lab) | VM 기반 K8s 테스트 클러스터 (multipass / libvirt backend) |
