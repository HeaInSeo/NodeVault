# Platform Schedule

버전: 3.4
갱신: 2026-07-12
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
| NodeVault | Phase 3 라이브 검증 중 발견된 build-context overlay `userxattr` 충돌(nouserxattr root overlay와 nested overlay) 수정 | issue [#2](https://github.com/HeaInSeo/NodeVault/issues/2) |
| NodeVault | in-Pod Buildah pull/push의 Harbor 자체서명 CA 신뢰 갭(`/etc/containers/certs.d` Secret 마운트) 수정 | issue [#3](https://github.com/HeaInSeo/NodeVault/issues/3) |
| NodeVault | Buildah build-context/scratch dir을 `/tmp` 전체가 아닌 전용 서브트리로 스코핑 | issue [#4](https://github.com/HeaInSeo/NodeVault/issues/4) |
| NodeVault | TODO-13 (sori 패키징 통합 경계) — `SORI_INTEGRATION_BOUNDARY.md` + `pkg/oras/referrer.go`로 기존 구현 완료 확인, 스케줄 추적 공백만 메움 | issue [#5](https://github.com/HeaInSeo/NodeVault/issues/5) (closed) |
| NodeVault | vendor된 btrfs graphdriver 등록 제거 — bare `go test ./...`가 `<btrfs/version.h>` 헤더 부재(Rocky/RHEL 미패키지)로 실패하던 문제 해결 | `bb2b172` |
| NodeVault | proto: `BuildKind` enum (TOOLSPEC/TOOLFUNCTIONSPEC) + `BuildRequest.kind`/`base_image_digest` 추가; `inputs/outputs/display/command` BuildRequest·RegisterToolRequest에서 reserve — ToolFunctionSpec 설계 문서 기반, 빌드 경로에서 분리 | `eedf523` |
| NodeSentinel | L3 dry-run, L4 smoke-run | Sprint 2 |
| NodeSentinel | L5-a functional validation + vaultclient | Sprint 3 |
| NodeSentinel | L5-b trivy-operator scan | Sprint 3 |
| NodePalette | GET /v1/palette/tools (PromotionStatus=active 필터) | Sprint 4 |

---

## 다음 작업: v0.1.0 아키텍처 구현

아키텍처 v0.1.0은 현재 `legacy BuildRequest` 경로를 `ToolSpecRequest → ResolvedToolSpec → SubmitToolBuild` 경로로 전환하는 것이 핵심이다. 섹션 16의 마이그레이션 단계를 기준으로 한다.

### Sprint 0 — NodeVault final build gate (issue #16)

**목표**: NodeKit L1 검증을 우회한 직접 gRPC 호출도 NodeVault에서 최종 차단한다. NodeKit은 authoring/UX 1차 gate이고, NodeVault는 raw spec을 실제 Buildah에 넘기기 전 authoritative build gate다.

**원칙**

- `ResolveToolSpec`은 digest/index 생성 단계이며 Dockerfile rewrite 단계가 아니다.
- NodeVault는 NodeKit이 보낸 `dockerfile_content`를 그대로 빌드하되, `builder.Build()` 전에 서버 쪽 정책 검증을 다시 실행한다.
- legacy `BuildAndRegister`가 열려 있는 동안에는 `SubmitToolBuild`와 동일한 build gate를 적용한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/resolve/digest.go` | base image digest를 `^sha256:[0-9a-fA-F]{64}$`로 검증 |
| `pkg/build/validate.go` | NodeVault final build gate 추가 |
| `pkg/build/submit_tool_build.go` | `buildRequestFromResolved` 이후, build state 생성 전 `ValidateBuildRequest` 실행 |
| `pkg/build/service.go` | legacy `BuildAndRegister`도 builder 호출 전 동일 정책 검증 |

**최소 Dockerfile 정책**

- 모든 `FROM`은 `@sha256:<64 hex>` digest pin 필수
- `latest` 태그 금지
- 모든 `FROM` digest 형식 검증
- `USER root`, `USER 0`, `USER 0:0`, `USER root:root`, root group, 변수 기반 `USER` 차단

**완료 판정**

- [x] 짧은 base image digest가 `ResolveToolSpec`에서 `InvalidArgument`로 거부됨
- [x] `SubmitToolBuild`가 invalid Dockerfile을 build state 생성 전 거부
- [x] `BuildAndRegister`가 invalid Dockerfile을 builder 호출 전 거부
- [x] `go test -tags "$(BUILDTAGS)" ./pkg/resolve ./pkg/build` 통과
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과

### Sprint 1 — Build gate contract formalization

**목표**: Sprint 0에서 추가한 final build gate를 NodeKit/NodeVault 사이의 공식 계약으로 고정한다.

**결정**

- 현재 NodeVault Dockerfile build path는 `BUILD_KIND_UNSPECIFIED`(legacy compatibility)와 `BUILD_KIND_TOOLSPEC`만 지원한다.
- `BUILD_KIND_TOOLFUNCTIONSPEC`은 proto/API 모델에는 존재하지만, function-image builder가 생기기 전까지 `ValidateBuildRequest`에서 명시적으로 거부한다.
- `ResolveToolSpec`은 raw spec digest/index 생성 단계다. Dockerfile rewrite, package pin rewrite, base image ref rewrite를 수행하지 않는다.
- NodeVault final gate는 Go native validator를 기준 구현으로 유지한다. DockGuard WASM 직접 실행은 정책 drift를 줄이기 위한 별도 후속 이슈 [#17](https://github.com/HeaInSeo/NodeVault/issues/17)로 분리한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/build/validate.go` | `BuildKind`별 gate 계약 명시 (`UNSPECIFIED`/`TOOLSPEC` 허용, `TOOLFUNCTIONSPEC` 거부) |
| `protos/nodevault/v1/nodevault.proto` | BuildRequest 주석으로 현 build path 지원 범위 명시 |
| `docs/TOOL_NODE_SPEC.md` | NodeKit L1 / NodeVault final gate / ResolveToolSpec 역할 분리 |
| `docs/PLATFORM_MAP.md` | DockGuard와 NodeVault native final gate의 현재 관계 명시 |

**완료 판정**

- [x] `TOOLFUNCTIONSPEC`이 현재 Dockerfile build path에서 명시적으로 거부됨
- [x] `UNSPECIFIED` legacy request는 `TOOLSPEC`으로 취급됨
- [x] proto/doc/code가 `ResolveToolSpec != Dockerfile rewrite` 계약을 동일하게 설명
- [x] DockGuard WASM 직접 실행 후속 이슈 생성 또는 기존 이슈에 추적 항목 추가 — issue [#17](https://github.com/HeaInSeo/NodeVault/issues/17)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과

### Sprint 2 — ResolveRecipe / Submit reproducibility contract

**목표**: `ResolveRecipe`와 `SubmitToolBuild` 사이의 package pinning 책임을 명확히 한다.

**결정**

- `ResolveRecipe`는 candidate lookup API다. Harbor cache 또는 외부 source에서 `BuildStringCandidate`를 반환하지만 `raw_spec`이나 Dockerfile을 rewrite하지 않는다.
- NodeKit은 사용자 선택 결과를 `dockerfile_content` 또는 `environment_spec`에 반영해 `ResolveToolSpec`/`SubmitToolBuild` 경로로 제출한다.
- NodeVault는 Submit/Build gate에서 실제 제출된 spec이 재현 가능한 full pin인지 검증한다.
- conda/mamba/micromamba 패키지는 `name=version=build` 형식을 요구한다. NodeKit L1의 `name=version` 허용은 UX 단계의 최소 입력 검증이며, NodeVault final gate를 대체하지 않는다.
- NodeVault가 canonical resolved spec을 생성하는 API는 현재 범위 밖이다. 필요해지면 별도 API/이슈 [#18](https://github.com/HeaInSeo/NodeVault/issues/18)로 분리한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/build/validate.go` | Dockerfile `RUN conda/mamba/micromamba install` 및 `environment_spec` package full pin 검증 |
| `protos/nodevault/v1/nodevault.proto` | `ResolveRecipe`가 candidate lookup이고 rewrite가 아님을 주석화 |
| `docs/PLATFORM_SCHEDULE.md` | ResolveRecipe/Submit 책임 분리 기록 |

**완료 판정**

- [x] Dockerfile 내 `conda install bwa=0.7.17` 같은 version-only pin이 build gate에서 거부됨
- [x] Dockerfile 내 `micromamba install bwa=0.7.17=h5bf99c6_8` 같은 full pin이 허용됨
- [x] `environment_spec` 내 version-only conda pin이 거부됨
- [x] `ResolveRecipe`는 rewrite/canonical spec 생성 API가 아님을 proto/doc에 명시
- [x] canonical resolved spec 생성 필요 여부를 후속 이슈로 추적 — issue [#18](https://github.com/HeaInSeo/NodeVault/issues/18)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과

### Sprint 3 — ToolSpec / ToolFunctionSpec metadata contract

**목표**: NodeKit recipe/function metadata(`command`, `inputs`, `outputs`, `display`)가 build/register 경로에서 어디에 저장되는지 명확히 한다.

**결정**

- L2 build/register 경로는 ToolSpec image/environment metadata만 저장한다.
- `BuildRequest`와 `RegisterToolRequest`는 `inputs`, `outputs`, `display`, `command`를 받지 않는다. 해당 이름은 proto에서 reserved 상태다.
- `RegisteredToolDefinition`의 `inputs`, `outputs`, `display`, `command` 필드는 ToolFunctionSpec/function-validation path에서 populate될 필드이며 L2 build 시점에는 비워둔다.
- `toolspec` OCI referrer도 build-time ToolSpec metadata만 포함한다. function metadata를 이 referrer에 섞지 않는다.
- ToolFunctionSpec metadata 등록/검증 경로는 후속 이슈 [#19](https://github.com/HeaInSeo/NodeVault/issues/19)로 분리한다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/catalog/catalog.go` | build-time RegisterTool이 ToolSpec metadata만 저장한다는 주석 추가 |
| `pkg/catalog/catalog_test.go` | build-time RegisterTool이 function metadata를 populate하지 않음을 테스트 |
| `pkg/oras/referrer.go` | toolspec referrer payload 범위 주석 명시 |
| `protos/nodevault/v1/nodevault.proto` | RegisterToolRequest/RegisteredToolDefinition metadata 계약 주석 보강 |

**완료 판정**

- [x] build/register 경로에 저장되는 metadata 범위가 문서화됨
- [x] `command`, `inputs`, `outputs`, `display`가 L2 build-time RegisterTool에서 비어 있음을 테스트
- [x] ToolFunctionSpec metadata 후속 이슈 생성 — issue [#19](https://github.com/HeaInSeo/NodeVault/issues/19)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과

### Sprint 4 — Operational leftovers

**목표**: 남은 운영 이슈 중 NodeVault 저장소 안에서 처리 가능한 부분을 닫고, 외부 의존 항목을 명확히 분리한다.

**결정**

- issue #14의 NodeVault 측 책임은 NodePalette REST에서 `cas_hash`가 포함된 tool 목록을 제공하는 것이다. 실제 DagEdit 클라이언트가 이 값을 RunnerNode에 기록하는 작업은 DagEdit 쪽 소비자 구현이다.
- issue #13은 seoy live cluster 접근과 실제 동일 ToolSpec 2회 빌드가 필요하므로 코드 변경이 아니라 운영 검증 항목이다. 현재 세션에서는 `../infra-lab/kubeconfig`가 존재하지만 API server `192.168.122.99:6443`에 `no route to host`로 접근할 수 없어 라이브 검증을 보류한다.
- issue #6은 NodeKit UI revision 정책 합의가 필요한 설계 결정 항목이다. NodeVault index에는 아직 `stableRef -> current active casHash` 단수 포인터를 추가하지 않는다.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/catalogrest/server.go` | `/v1/palette/tools`, `/v1/palette/data` aliases 추가 |
| `pkg/catalogrest/server_test.go` | `/v1/palette/tools` 응답에 `cas_hash` 포함 테스트 |
| `docs/NODEPALETTE_DESIGN.md` | palette alias endpoint 문서화 |
| `docs/PLATFORM_SCHEDULE.md` | 운영 잔여 이슈 상태 정리 |

**완료 판정**

- [x] `GET /v1/palette/tools`가 active tool 목록을 반환함
- [x] `GET /v1/palette/tools` 응답에 `cas_hash` 포함
- [x] `GET /v1/palette/data` alias 추가
- [x] issue #13 라이브 검증 블로커 확인 — kubeconfig 있음, 현재 네트워크 `no route to host`
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과

---

## 재현성 개선 (P0~P3, 2026-07-08 라이브 테스트 기반)

NodeKit→NodeVault 라이브 재현성 테스트(`docs/NODEKIT_LIVE_RECIPE_REPRO_TEST_2026-07-08.md`, `docs/NODEKIT_LIVE_RECIPE_EXTENDED_TEST_2026-07-08.md`) 결과, 핵심 빌드 경로(NodeKit recipe → ResolveToolSpec/SubmitToolBuild → in-Pod Buildah → Harbor push → digest 기록 → index Active)는 정상 동작함을 확인했다. 남은 4가지 항목(`docs/NODEKIT_NODEVAULT_REPRO_IMPROVEMENT_NODEVAULT.md` P0~P3)을 Sprint 5~11로 분해한다.

### Sprint 5 — P0a: RegistryConfig 통합 타입 도입

**목표**: Buildah push, ORAS referrer push, reconcile, digest resolve 4개 경로가 각자 읽던 registry 설정(scheme/CA/auth)을 단일 `pkg/registryconfig` 타입으로 통합한다. 이 스프린트는 타입 도입 + ORAS 소비자 전환까지; reconcile 전환은 Sprint 6.

**결정**

- 새 패키지 `pkg/registryconfig`(leaf 패키지, `pkg/oras`↔`pkg/registry` 상호 의존 없음을 확인 완료 — 양쪽에서 안전하게 임포트 가능).
- `Config{Addr, Scheme, CAFile, AuthFile, InsecureTLS}`. `FromEnv()`: `NODEVAULT_REGISTRY_ADDR`(기존 `pkg/build/service.go`의 `defaultRegistryAddr` 이관), `NODEVAULT_REGISTRY_SCHEME`(default `"https"`), `NODEVAULT_REGISTRY_CA_FILE`(없으면 `NODEVAULT_ORAS_CA_FILE`로 폴백), `REGISTRY_AUTH_FILE`(default `/run/containers/0/auth.json`).
- CA 자동탐색: 둘 다 비어있으면 `/etc/containers/certs.d/<host>/ca.crt` 존재 확인 — 이는 Buildah(containers/image 라이브러리)가 이미 자동 신뢰하는 경로(`deploy/03-nodevault.yaml`의 `nodevault-harbor-ca` Secret 마운트)이므로 **`pkg/build/builder.go`는 코드 변경 없이** AC-REG-01을 만족한다.
- `HTTPClient()`의 TLS RootCAs는 `x509.SystemCertPool()` + `CAFile` 추가(교체 아님) — 공인 레지스트리(docker.io 등) 조회를 깨지 않으면서 Harbor 자체서명 CA도 신뢰.
- `pkg/oras/referrer.go`의 `credentialsFromAuthFile`을 `registryconfig.Config.Credentials(host)`로 순수 이동(로직 변경 없음).

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/registryconfig/config.go` (신규) | `Config`, `FromEnv()`, `discoverCAFile(addr)`, `HTTPClient()`, `Credentials(host)` |
| `pkg/registryconfig/config_test.go` (신규) | env 조합별 `FromEnv()` 검증, CA 자동탐색 유무 검증 |
| `pkg/oras/referrer.go` | `newRemoteRepository`가 `registryconfig.FromEnv()` 사용; `credentialsFromAuthFile` 삭제 후 이관된 함수 호출로 대체 |
| `pkg/oras/referrer_test.go` | auth-file 파싱 테스트를 이관에 맞게 갱신(동작 동일성 회귀 확인) |
| `pkg/build/service.go` | `defaultRegistryAddr` 상수를 `pkg/registryconfig`로 이동, `registryAddr()`는 `registryconfig.FromEnv().Addr` 위임 |
| `deploy/03-nodevault.yaml` | `NODEVAULT_REGISTRY_SCHEME=https`, `NODEVAULT_REGISTRY_CA_FILE` env 추가(기존 `NODEVAULT_REGISTRY_ADDR` 옆) |
| `README.md`, `ARCHITECTURE.md` | 신규 env var 문서화 |

**완료 판정**

- [x] `TestRegistryConfig_FromEnv_Defaults`
- [x] `TestRegistryConfig_DiscoverCAFile`
- [x] `TestRegistryConfig_ORASCAFileFallback`
- [x] `pkg/oras` 기존 테스트 전부 통과(회귀 없음)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음(신규 코드 0경고 — 이번 스프린트 이전부터 있던 사전 존재 경고 2건은 범위 밖)

### Sprint 6 — P0b: reconcile HTTPS 전환 + 401/타임아웃 구분 + AC-REG-04

**목표**: `pkg/registry/checker.go`의 하드코딩된 `http://`를 제거하고 `registryconfig`를 사용하도록 전환한다. HTTP 401(인증 챌린지)이 "not found"로 오분류되던 버그를 고친다. referrer push 실패 시에도 reconcile을 즉시 트리거해 `integrity_health=Partial`이 신속히 반영되도록 한다(AC-REG-04).

**결정**

- `HarborChecker`는 `NewHarborChecker(cfg registryconfig.Config) (*HarborChecker, error)`로 변경(CA 파일을 못 읽으면 생성 시점에 실패 — 유일한 프로덕션 호출부는 `cmd/controlplane/main.go`의 `startBackground`, 실패 시 `run()`이 종료 코드 1 반환). URL 조립을 `cfg.Scheme`로, `cfg.HTTPClient()`로 CA-신뢰 클라이언트 사용.
- Outcome 분류는 기존 `(bool, error)` 시그니처 유지: `200`→`(true,nil)`, `404`→`(false,nil)`(확정 not-found), 그 외(401/403/5xx/타임아웃/TLS 실패)→`(false, err)`(indeterminate, `SetIntegrityHealth` 호출 안 함 — 기존 보수적 에러 경로 재사용).
- 401은 `pkg/registry/resolve.go`의 기존 `parseBearerChallenge`/`anonymousToken`을 재사용해 익명 토큰 1회 재시도 후에도 실패하면 명시적 에러 반환.
- AC-REG-04: `pkg/build/service.go`의 `postBuildRegistration`에서 `ReconcileOne` 호출을 referrer push 성공/실패 공통 경로로 이동. 이는 reconcile의 기존 계산 경로를 조기 트리거하는 것이지 `pkg/build`가 `integrity_health`를 직접 쓰는 게 아니다 — CLAUDE.md 이중 축 규칙 위반 아님.
- 함께 정리: `pkg/registry/registry.go`의 `Client.GetDigest`(프로덕션 미사용 dead code, 동일하게 `http://` 하드코딩)도 이 스프린트에서 삭제.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/registry/checker.go` | `NewHarborChecker(cfg registryconfig.Config)`; `http://` 하드코딩 제거; 401 챌린지 처리 |
| `pkg/registry/checker_test.go` | 200/404/401/타임아웃 outcome 검증 |
| `cmd/controlplane/main.go` | `registryconfig.FromEnv()` 전달 |
| `pkg/build/service.go` | `postBuildRegistration` reconcile 트리거 경로 통합 |
| `pkg/build/service_test.go` | AC-REG-04 회귀: referrer 실패해도 `ReconcileOne` 호출 + `lifecycle_phase` Active 유지 |
| `pkg/registry/registry.go`, `registry_test.go` | dead code(`Client.GetDigest`) 삭제 |

**완료 판정**

- [x] `TestHarborChecker_ImageExists_404_NotFound`
- [x] `TestHarborChecker_ImageExists_401_IsNotNotFound`
- [x] `TestHarborChecker_ImageExists_401WithChallenge_RetriesWithAnonymousToken`(익명 토큰 재시도 경로 자체도 검증)
- [x] `TestHarborChecker_UsesConfiguredScheme`
- [x] `TestPostBuildRegistration_ReferrerPushFailure_TriggersReconcile`
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음(신규 코드 0경고 — 사전 존재 경고 2건은 범위 밖)

### Sprint 7 — P1a: build_state 아티팩트 메타데이터 브릿지

**목표**: `pkg/buildstate.Record`에 image_ref/image_digest/spec_referrer_digest/integrity_health를 추가하고 `WatchToolBuild`가 스트리밍하도록 한다(AC-EVT-02).

**결정**

- 마이그레이션: 버전 관리 프레임워크 신설 없이, `init()`에서 `PRAGMA table_info(build_state)`로 컬럼 존재 확인 후 없으면 `ALTER TABLE ... ADD COLUMN ... DEFAULT ''` 실행하는 `ensureColumn` 헬퍼(멱등적, 매 `Open()`마다 실행). Sprint 8에서 재사용.
- `IntegrityHealth`는 `pkg/buildstate`가 `pkg/index`를 임포트하지 않는 현재 경계를 유지하기 위해 plain string 필드. read-through 스냅샷: `ReconcileOne` 호출 직후 `indexStore.GetByCasHash`(읽기 전용)로 방금 계산된 값을 복사만 함 — `pkg/buildstate`/`pkg/build` 모두 `SetIntegrityHealth`를 직접 호출하지 않음(이중 축 규칙 준수 재확인).
- `BuildEvent` proto에 `image_ref`, `image_digest`, `spec_referrer_digest`, `integrity_health` 필드 추가.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/buildstate/store.go` | `Record` 필드 추가, `ensureColumn` 헬퍼, `SetArtifact`, `SetReferrer` 신규 메서드 |
| `pkg/buildstate/store_test.go` | 신규 컬럼 CRUD + 구 스키마 DB 마이그레이션 테스트 |
| `protos/nodevault/v1/nodevault.proto` | `BuildEvent`에 필드 추가 |
| `pkg/build/submit_tool_build.go` | `runSubmittedBuild`(digest 획득 시 `SetArtifact`), `buildStateEvent` 신규 필드 채우기 |
| `pkg/build/service.go` | `postBuildRegistration`에서 referrer 처리 후 `SetReferrer` 호출 |

**완료 판정**

- [x] `TestBuildStateStore_SetArtifact_PersistsImageRefDigest`
- [x] `TestBuildStateStore_SetReferrer_PersistsReferrerAndIntegrityHealth`
- [x] `TestBuildStateStore_EnsureColumn_MigratesExistingDB`(구 스키마 DB 파일을 직접 만들어 `Open()`이 in-place 마이그레이션하는지 확인)
- [x] `TestWatchToolBuild_ExposesImageDigest`(AC-EVT-02)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음(신규 코드 0경고 — 사전 존재 경고 2건은 범위 밖)

### Sprint 8 — P1b: durable build_events 테이블 + 이벤트 종류 확장

**목표**: 빌드 생애주기 전체의 구조화된 이벤트 로그를 영속화(AC-EVT-01), NodeSentinel/NodePalette가 로그 스크레이핑 없이 추적 가능한 기반 마련(AC-EVT-03 기반).

**결정**

- `build_events` 테이블(1:N, `build_state`와 별개): `id, build_id, kind, message, image_ref, image_digest, spec_referrer_digest, integrity_health, created_at`.
- `BuildEventKind`에 `BUILD_SUBMITTED, BUILDING, PUSHING, SPEC_REFERRER_PUSHED, SPEC_REFERRER_PARTIAL, CANCELLED` 추가(기존 값 번호 재사용 안 함, 이어붙여 레거시 `BuildAndRegister` 스트림 호환성 유지).
- `WatchToolBuild`의 폴링 메커니즘은 이 스프린트에서 변경하지 않는다 — `build_events`는 영속 기록/감사용, 실시간 스트리밍은 Sprint 7의 브릿지 필드로 충분. 이벤트 로그를 gRPC로 노출하는 신규 RPC는 범위 밖.
- `AppendEvent`는 `Transition`과 별도 호출(상태 전이와 이벤트 기록 실패를 격리 — 실패 시 `slog.Warn`만).

**주요 작업**

| 파일 | 변경 |
|------|------|
| `protos/nodevault/v1/nodevault.proto` | `BuildEventKind`에 6개 값 추가 |
| `pkg/buildstate/store.go` | `build_events` 테이블, `AppendEvent(...)`, `ListEvents(buildID)` |
| `pkg/buildstate/store_test.go` | append/list, build_id 필터링 |
| `pkg/build/submit_tool_build.go` | 각 상태 전이 지점에 대응 `AppendEvent` 호출 |
| `pkg/build/service.go` | `recordBuildSuccess`(PUSH_SUCCEEDED, DIGEST_ACQUIRED), `postBuildRegistration`(SPEC_REFERRER_PUSHED/PARTIAL), `CancelToolBuild`(CANCELLED) |

**완료 판정**

- [ ] `TestBuildStateStore_AppendEvent_ListEvents`
- [ ] `TestSubmitToolBuild_PersistsPushSucceededAndDigestAcquired`(AC-EVT-01)
- [ ] `TestPostBuildRegistration_ReferrerFailure_PersistsSpecReferrerPartial`
- [ ] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음

### Sprint 9 — P2a: SourceBuild 정적 Dockerfile 정책 (risky RUN 탐지 + allowRuntimeTools)

**목표**: 최종 스테이지 `RUN` 라인에서 risky runtime tool을 명시적으로 설치/실행하는 패턴을 빌드 게이트에서 차단한다.

**범위 고지**: 이 스프린트는 정적 텍스트 스캔만 구현한다. base 이미지가 이미 curl/wget을 포함하는 경우(`curlimages/curl` 등 — 실제 라이브 테스트 실패 케이스)는 탐지 불가 → AC-SB-01, AC-SB-03(부분)만 만족, **AC-SB-02/04는 Sprint 10 필요**.

**결정**

- `riskyRuntimeTools`: `curl, wget, git, ssh, scp, apt, apt-get, apk, yum, dnf, gcc, g++, clang, make, cmake`. **원 요구사항 문서의 목록에서 `mamba`/`conda`/`micromamba`를 제외했다** — 구현 중 발견: Conda/Micromamba 레시피 variant는 `RUN micromamba install <pkg>=<version>=<build>` 자체가 빌드 메커니즘이며(라이브 재현 테스트 p04/p05에서 PASS로 확인된 정상 경로), 이걸 risky로 걸면 지금 동작하는 빌드가 전부 막힌다. NodeKit `docs/NODEKIT_SOURCEBUILD_STRUCTURED_INTENT_DESIGN.md`(2026-07-12)를 확인한 결과 멀티스테이지 렌더링은 `SourceBuild` 레시피에만 계획돼 있고(Phase B/C, 구현 미착수) Conda/Micromamba는 대상이 아니다 — NodeKit이 이 레시피들도 멀티스테이지로 내거나 `allow_runtime_tools`를 채워 보내게 되면 그때 다시 추가한다.
- 마지막 `FROM` 이후의 `RUN`만 검사(중간 빌드 스테이지는 허용) — `validateDockerfilePolicy`의 기존 `fromCount` 스테이지 경계 추적 재사용(정확히는 전체 `FROM` 개수를 먼저 센 뒤 마지막 스테이지인지 비교).
- 매 RUN을 `&&`/`||`/`|`/`;`로 분리한 각 커맨드 세그먼트의 첫 토큰이 risky 목록에 있는지 검사 — `RUN apt-get install curl`은 `apt-get`(그 자체가 risky) 토큰으로, `RUN curl ...`은 `curl` 토큰으로 자연스럽게 둘 다 잡힌다.
- `allowRuntimeTools`/`allowRuntimeToolsReason`을 `BuildRequest` proto 필드 18, 19로 추가(17까지 사용 중, `reserved 7,8,12,13,14,15` 확인). 위반 tool이 allow list에 있고 reason이 비어있지 않을 때만 통과.
- 최종 거부 판단은 NodeVault(`ValidateBuildRequest`)에서만 수행 — NodeKit L1 UI/검증 구현은 범위 밖(CLAUDE.md §1).

**주요 작업**

| 파일 | 변경 |
|------|------|
| `protos/nodevault/v1/nodevault.proto` | `BuildRequest`에 `allow_runtime_tools=18`, `allow_runtime_tools_reason=19` 추가 |
| `pkg/build/validate.go` | `riskyRuntimeTools` 목록, 전체 `FROM` 개수 사전 계산 후 최종 스테이지 판별, `validateFinalStageRuntimeTools(...)`, `shellSegments(...)` |
| `pkg/build/validate_test.go` | AC-SB-01/03(부분) 각각 테스트 |
| `pkg/build/submit_tool_build_test.go` | `allow_runtime_tools`/`reason`이 `raw_spec` JSON에서 정상 역직렬화되는지 회귀 테스트 |

**완료 판정**

- [x] `TestValidateBuildRequest_FinalStageRiskyTool_Rejected`
- [x] `TestValidateBuildRequest_AllowRuntimeToolsWithReason_Passes`
- [x] `TestValidateBuildRequest_AllowRuntimeToolsWithoutReason_Rejected`
- [x] `TestValidateBuildRequest_BuildStageRiskyTool_NotFinalStage_Passes`
- [x] `TestValidateBuildRequest_CleanFinalImage_NoCurl_Passes`
- [x] `TestBuildRequestFromResolved_DeserializesAllowRuntimeTools`
- [x] 기존 `TestValidateBuildRequest_AcceptsCondaInstallWithBuildString` 회귀 없음(conda/mamba/micromamba 제외로 확인)
- [x] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음(신규 코드 0경고 — 사전 존재 경고 2건은 범위 밖)

### Sprint 10 — P2b: post-build 최종 이미지 콘텐츠 스캔

**목표**: Sprint 9가 놓치는 "base 이미지가 이미 risky tool 포함" 케이스를 실제 빌드된 이미지에서 탐지한다(AC-SB-04 완전 충족, AC-SB-02 완결).

**선행 조건**: ~~`HeaInSeo/podbridge5` issue #2~~ — **해결됨(v0.1.8, 2026-07-13)**. `SaveImage(ctx, path, imageName, imageID, compress)`/`ImageArchivePath(basePath, imageName, compress)`가 공개 API로 노출되어 NodeVault vendor(v0.1.9)에 이미 포함되어 있다. 더 이상 이 스프린트를 막는 선행 조건이 없다 — issue [#32](https://github.com/HeaInSeo/NodeVault/issues/32)로 추적.

**결정**

- exec 기반 스캔(이미지 안에서 `which curl` 실행)은 채택하지 않는다 — 신뢰 안 되는 이미지 콘텐츠 실행은 새로운 공격 표면.
- 대신 tar 아카이브 export 후 엔트리 경로 스캔(`usr/bin/curl` 등 표준 PATH 하위 경로와 risky tool 이름 매칭) — mount+stat보다 podbridge5의 기존 구현과 정확히 맞고, 특권 마운트 연산도 불필요.
- `Builder` 인터페이스(`pkg/build/builder.go`)에 `InspectRiskyTools(ctx, imageID string, riskyTools []string) (found []string, err error)` 추가. `disabledBuilder`는 빈 슬라이스 반환(스캔 skip, WARN 아님).
- WARN vs FAIL 구분: allow list에 없는 risky tool 발견 시 FAIL(빌드 거부, push 이전에 중단), allow list에 있고 reason 있으면 WARN.
- 스캔 시점: `s.builder.Build(...)` 성공 직후, push 이전.

**주요 작업**

| 파일 | 변경 |
|------|------|
| (podbridge5 리포지토리, 별도 PR) | `SaveImage`(공개) 또는 `ExportImageArchive` 신규 공개 API — issue #2 |
| `pkg/build/builder.go` | `Builder` 인터페이스에 `InspectRiskyTools` 추가, tar 엔트리 스캔 구현 |
| `pkg/build/submit_tool_build.go` | build 성공 직후 스캔 호출, FAIL 시 push 이전 중단 |
| `pkg/build/builder_test.go` | fake export 기반 유닛 테스트 |

**완료 판정**

- [ ] `TestInspectRiskyTools_DetectsBaseImageTool`(AC-SB-04 FAIL 케이스)
- [ ] `TestInspectRiskyTools_AllowListedTool_WarnNotFail`(AC-SB-04 WARN 케이스)
- [ ] `TestRunSubmittedBuild_RiskyToolFail_BlocksPush`
- [ ] seoy 라이브 검증: curlimages/curl 기반 SourceBuild 재현 → FAIL 확인(별도 이슈로 트래킹)
- [ ] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음

### Sprint 11 — P3: pinning_status / reproducibility_status

**목표**: `name=version=build`(FullPin)와 `name=version`(VersionOnly)을 구분 표현한다. 정책은 배관(plumbing)만 — `validateCondaPackagePin`의 현재 hard-reject 동작은 그대로 유지한다.

**결정**

- `PinningStatus`/`ReproducibilityStatus`는 `LifecyclePhase`/`IntegrityHealth`와 같은 선례(plain string 필드, proto enum 아님)를 따른다 — 클라이언트가 형태화하는 값이 아니라 서버가 계산하는 상태값이기 때문.
- 저장 위치: `pkg/index.ResolvedToolSpec`(resolve 시점) + `pkg/index.Entry`(등록 시점 복사) 양쪽.
- `validateCondaPackagePin`을 `error` → `(PinningStatus, error)` 반환으로 리팩터: `=` 2개(FullPin), `=` 1개(VersionOnly, 계속 reject), `$` 변수(Unresolved, 계속 reject). 호출자가 여러 패키지 판정 중 가장 약한 값으로 집계.
- `VersionOnly`를 통과시키는 "loose mode"(AC-PIN-03)는 **결정됨(2026-07-14, 사용자 확인)**: 도입하지 않는다. `validateCondaPackagePin`의 hard-reject(name=version=build 미만 전부 거부)를 계속 유지 — 재현성을 타협하지 않는 원칙을 지킨다. NodeKit도 이 결정을 인지하고 있으며(legacy SourceBuild 경고와 동일하게, full pin을 유도하는 UX로 대응 중), 이 스프린트가 실제 진행되어도 accept/reject 동작 자체는 바뀌지 않는다 — `PinningStatus`/`ReproducibilityStatus` 필드는 순수 배관(관측용)이다.
- `docs/INDEX_SCHEMA.md`가 schema_version 1에 멈춰 있고 실제 코드는 `schemaVersion=3`(`pkg/index/store.go`)인데 이 문서 앞부분은 같은 변경을 "Index v4"로 라벨링한 기존 불일치가 있음 — 이 스프린트가 새 필드를 추가하며 라벨을 실제 코드값과 맞출 것.

**주요 작업**

| 파일 | 변경 |
|------|------|
| `pkg/index/schema.go` | `PinningStatus`, `ReproducibilityStatus` 타입+상수; `ResolvedToolSpec`, `Entry`에 필드 추가 |
| `pkg/index/store.go` | 관련 mutator, `schemaVersion` 정리 |
| `pkg/build/validate.go` | `validateCondaPackagePin` 리팩터 + 집계 로직 |
| `pkg/build/resolve_tool_spec.go` | 집계된 `PinningStatus`를 `ResolvedToolSpec`에 기록 |
| `pkg/catalog/*.go` | 등록 시 `Entry.PinningStatus`/`ReproducibilityStatus` 계산 |
| `protos/nodevault/v1/nodevault.proto` | `RegisteredToolDefinition`에 `pinning_status`, `reproducibility_status` 추가 |
| `docs/INDEX_SCHEMA.md` | schema_version 최신화, 라벨 불일치 각주 명시 |

**완료 판정**

- [ ] `TestValidateCondaPackagePin_ClassifiesFullPinVsVersionOnly`(AC-PIN-01)
- [ ] `TestResolveToolSpec_VersionOnlySubmission_NeverComplete`(AC-PIN-02)
- [ ] `TestRegisterTool_PinningStatus_CopiedFromResolvedSpec`
- [ ] AC-PIN-03(loose mode)은 이번 스프린트 범위 밖 — NodeKit 합의 후 별도 스프린트로 추적
- [ ] `go test -tags "$(BUILDTAGS)" ./...` 통과, `make lint` 경고 없음

### 재현성 개선 스프린트 ↔ 요구사항 매핑

| 스프린트 | 항목 | AC 충족 |
|---|---|---|
| 5 | P0a RegistryConfig 타입 | AC-REG-01 |
| 6 | P0b reconcile HTTPS + 401 구분 + AC-REG-04 | AC-REG-02, AC-REG-04 (AC-REG-03은 이미 구현됨, 회귀 테스트만) |
| 7 | P1a build_state 브릿지 | AC-EVT-02, AC-EVT-03 기반 |
| 8 | P1b durable build_events | AC-EVT-01 |
| 9 | P2a 정적 Dockerfile 정책(conda/mamba/micromamba 제외) | AC-SB-01, AC-SB-03(부분) — AC-SB-02는 base 이미지 자체를 봐야 해서 Sprint 10 전담 |
| 10 | P2b post-build 콘텐츠 스캔 (podbridge5 issue #2 선행) | AC-SB-02(완결), AC-SB-04 |
| 11 | P3 pinning/reproducibility (plumbing만) | AC-PIN-01, AC-PIN-02 (AC-PIN-03은 NodeKit 합의 후 별도) |

---

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
- [x] registry 조회가 필요한 base image tag → digest resolve 구현 완료. `NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE=true`일 때만 활성화되는 operator opt-in이며, 기본값(off)은 기존 strict reject 동작을 그대로 유지한다. `pkg/registry.Client.ResolveTagDigest`가 HTTPS를 우선 시도하고 `WWW-Authenticate: Bearer` 401 challenge를 anonymous token으로 처리하며, HTTPS 연결 자체가 불가능할 때만 (TLS 없는 내부 Harbor 대응) plain HTTP로 fallback한다 — Harbor와 공개 레지스트리(docker.io/ghcr.io/quay.io)를 별도 코드 경로 없이 단일 구현으로 처리. 새 vendored dependency 없음 (순수 net/http). `pkg/build/resolve_tool_spec.go`의 `ResolveToolSpec`이 `resolve.BaseImagePin`으로 unpinned ref를 감지하면 이 resolver를 호출해 `resolve.Context.BaseImageDigest`를 채운다. 알려진 제한: ref가 `host/name:tag` 형태여야 하며 (Docker의 암묵적 `docker.io` 정규화는 미구현), 호스트가 없는 짧은 ref(예: `alpine:3.20`)는 거부된다.

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
- [x] `TestBuildCancel_CleansUpSubprocess` 통과 — simulated subprocess builder로 cancel 시 cleanup + active map 누수 없음을 검증. issue [#7](https://github.com/HeaInSeo/NodeVault/issues/7) (closed)
- [x] graphroot가 PVC에 유지됨 (Pod restart 후 layer cache hit 확인) — seoy에서 `rollout restart` 전후 graphroot `layers.json`/레이어 디렉터리가 byte-identical하게 유지됨을 직접 확인, 재시작 직후 `TestBuildAndRegister_SimpleDockerfile` 실제 빌드 성공. issue [#7](https://github.com/HeaInSeo/NodeVault/issues/7) (closed)
- [x] `go test ./...` 전체 통과 — 2026-06-23 bare `go test ./...`가 vendor된 btrfs 드라이버의 `<btrfs/version.h>` 헤더 부재로 실패하던 문제를 vendor 패치로 제거, 이제 빌드 태그 없이도 전체 통과 (commit `bb2b172`)

---

### Phase 3 — Rootless UserNS 전환 검증

**목표**: 대표 ToolSpec에 대해 `hostUsers: false`, `privileged: false`로 Buildah 빌드가 실제로 동작하는지 seoy(100.123.80.48) 클러스터에서 확인한다. (아키텍처 v0.1.0 §7)

**2026-06-22 갱신**: seoy 클러스터에서 라이브 검증을 수행해 `TestBuildAndRegister_SimpleDockerfile`이 end-to-end로 처음 성공했다. 검증 과정에서 실제 블로커 3건을 발견·수정했다 — build-context overlay `userxattr` 충돌(issue #2), Harbor 자체서명 CA 미신뢰(issue #3), Buildah scratch/context dir이 `/tmp` 전체를 덮던 문제(issue #4). 아래 검증 매트릭스는 이 결과를 반영해 갱신했다.

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

- [x] seoy에서 `make deploy-infralab` 성공
- [x] 대표 Dockerfile (alpine-based) `privileged: false`로 빌드 성공 (`TestBuildAndRegister_SimpleDockerfile`)
- [x] `ToolBuildRecord.Backend == "in-pod-buildah"` 확인 (단위 테스트 `pkg/build/service_test.go` + 라이브 통합 테스트로 확인)
- [x] Harbor에 image push 및 imageDigest 기록 확인 (issue #3 수정 후 라이브로 확인)
- [x] rootless 실패 시 `BuildEventKind_FAILED` 반환 (fallback 없음) 확인 — `TestBuildAndRegister_RootlessFailure_NoPrivilegedFallback`: rootless EPERM 에러 주입 시 FAILED 이벤트 발생, SUCCEEDED 없음, Build 정확히 1회 호출(재시도 없음) 검증. builder.go/service.go/submit_tool_build.go 코드 리뷰로 privileged retry 경로 부재 확인.

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

- [x] package cache PVC (`nodevault-package-cache`) + CONDA_PKGS_DIRS/MAMBA_PKG_CACHE 마운트 (2026-06-28)
- [x] Harbor layer cache (`NODEVAULT_BUILD_CACHE_REF` → `podbridge5.CacheRef`) 연결 — operator opt-in (2026-06-28)
- [x] `BuildExecution.CacheRef` + `LayerCacheHit` 필드 추가 (2026-06-28)
- [ ] 동일 ToolSpec 두 번째 빌드에서 `LayerCacheHit: true` seoy 라이브 검증
- [x] cache GC — `pkg/cachegc`: high watermark 기반 eviction, oldest-mtime-first LRU, background loop, 11개 단위 테스트 (2026-06-29)
  - bug fix: `RunOnce()` watermark ≤ 0 가드 누락 → 전체 캐시 삭제 위험 (2026-06-30)
  - bug fix: sub-MiB 엔트리 `sizeMiB=0` truncation → eviction 루프가 목표 달성 불가로 전체 삭제 (2026-06-30)

---

### Phase 5 — Record와 Certification 통합 (현재 구현 검증)

**목표**: 현재 구현된 ValidationResultService / certification.Service / CertifiedToolImageRecord 흐름이 아키텍처 v0.1.0 §12와 일치하는지 확인하고, 누락 필드를 보완한다.

**확인 항목**

| 현재 구현 | v0.1.0 요구 | 상태 |
|-----------|-------------|------|
| `ToolBuildRecord.Backend` | `execution.mode` / `execution.hostUsers` 등 분리 필요 | 구현 완료 (`mode`, `host_users`, `storage_driver`, `isolation`) |
| `ToolBuildRecord.NanVersion` | v0.1.0에 nan 미정의 — 필드 용도 재검토 | 제거 완료. ARCHITECTURE_V01.md에 `nan`/node-artifact-runtime 개념이 전혀 정의되어 있지 않아, 실제 사양이 생기기 전까지 필드·`Builder.Build()` 반환값·관련 테스트를 모두 제거했다. 필요해지면 사양과 함께 재도입한다. |
| `CertifiedToolImageRecord` 키 | `toolSpecDigest + platform` | 구현 완료. platform 없는 과거 record는 imageDigest compatibility lookup 유지 |
| `ToolScanRecord.DbDigest` | scanner + scannerVersion + dbDigest | 구현 완료 (gRPC + REST ingestion) |
| Harbor OCI referrer | spec referrer / toolprofile referrer | toolspec 구현 완료; toolprofile push 구현 완료 (`PushToolProfileReferrer` + `ObservedProfileDigest`); retention(latest 3개, index-local GC marking) 구현 완료 (`docs/OBSERVED_PROFILE_SPEC.md` §5) |

**완료 판정**

- [x] `ToolBuildRecord`에 `execution.*` 필드 추가 (backward-compatible optional)
- [x] `CertifiedToolImageRecord` 키를 `toolSpecDigest + platform`으로 재정렬 (Phase 1 이후)
- [x] `TestToolScanRecord_WithDbDigest` 통과
- [x] `pkg/profiler` 신규 — `ComputeValidationHash` (환경 독립 SHA256) + `IsInfraFailure`/`ClassifyFailure` 분류기 (2026-06-28)
- [x] 9개 profiler 단위 테스트 통과 (hash 결정론·환경값 제외·infra/timeout 분류) (2026-06-28)

---

### Phase 6 — Legacy API 축소

**목표**: `BuildRequest / BuildAndRegister`를 deprecate하고 신규 경로로 전환한다. (아키텍처 v0.1.0 §10.3)

이 단계는 NodeKit이 `ToolSpecRequest` 경로를 완전히 채용한 이후에 진행한다. **2026-07-21 재확인**: NodeKit이 2026-07-14(GUI, commit 9ccda57 "migrate Avalonia GUI off legacy BuildAndRegister")·2026-07-16(전체 재검증, commit e572a1b — `CS0618`/Obsolete 경고 0건 확인)에 완전히 전환 완료. `GrpcBuildClient`/`IBuildClient`와 그 테스트는 저장소에서 삭제됐고, `BuildAndRegister`를 실제로 호출하는 코드는 NodeKit 어디에도 없다(주석·proto 정의만 남음) — 2026-07-07 노트는 stale. 실제 RPC 제거 여부는 이슈 [#15](https://github.com/HeaInSeo/NodeVault/issues/15)에서 릴리스 정책으로 결정한다.

**전환 원칙**

```text
Legacy BuildRequest
  → compatibility adapter
  → ToolSpecRequest draft 생성
  → NodeVault resolve
```

**완료 판정**

- [ ] NodeKit legacy BuildRequest usage 0
- [x] `BuildAndRegister` RPC가 deprecated 표시 상태
- [x] legacy `BuildAndRegister` 호출 시 warning 로그 기록
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

**구현 저장소: NodeSentinel** — L5-a 실행 로직(Job 생성, ConfigMap 마운트, output 캡처, observedIoProfile 수집)은 NodeSentinel 도메인이다. NodeVault는 `pkg/validation/service.go`의 `SubmitToolCheckRecord` RPC로 결과를 수신한다. issue [#9](https://github.com/HeaInSeo/NodeVault/issues/9) (closed)

| 파일 | 변경 | 저장소 |
|------|------|--------|
| `pkg/worker/l5a.go` | sample fixture ConfigMap 마운트 + output emptyDir 수집 | NodeSentinel |
| `pkg/worker/l5a.go` | `observedIoProfile`: 포트별 파일 존재·개수·크기 | NodeSentinel |
| `pkg/worker/l5a.go` | `contractCheck`: 선언 output 존재 여부 | NodeSentinel |

---

### 병렬 트랙 C — 운영 안정화

| 항목 | 내용 | 우선순위 |
|------|------|---------|
| ValidateService RBAC 이관 | L3/L4 Job 권한 NodeVault SA → NodeSentinel SA | ✓ |
| Harbor 인증 Secret 통합 | buildah용 + ORAS용 분리 → 단일 정리 | ✓ |
| Data write path | DataRegisterRequest gRPC 경로 완성 | ✓ |
| DagEdit ↔ NodePalette 연결 | GET /v1/palette/tools → casHash pin | NodeVault alias 완료, DagEdit 소비자 연결은 외부 후속 |

**완료 판정**

- [x] DataRegisterRequest gRPC 경로 완성 — `DataRegistryService.RegisterData/GetData/ListData` + `cmd/controlplane/main.go:RegisterDataRegistryServiceServer` (2026-06-28)
- [x] ValidateService RBAC 이관 — `BuildAndRegister` L3/L4 직접 Job 생성 제거, `02-rbac.yaml` ClusterRole/Binding 제거, NodeSentinel EnqueueValidationWork 위임 (2026-07-02, issue [#11](https://github.com/HeaInSeo/NodeVault/issues/11))
- [x] Harbor 인증 Secret 통합 — `pkg/oras/referrer.go`가 `HARBOR_USER`/`HARBOR_PASS` 대신 auth.json(Buildah와 동일 파일) 파싱, `nodevault-harbor-auth` Secret 제거 (2026-07-02, issue [#12](https://github.com/HeaInSeo/NodeVault/issues/12))
- [x] NodePalette alias — `GET /v1/palette/tools`가 `GET /v1/catalog/tools`와 동일 schema를 반환하며 `cas_hash`를 포함 (NodeVault 측 issue #14 범위)

---

### 병렬 트랙 D — Recipe 재현성 해소 (ResolveRecipe)

**배경**: 사용자는 툴 이름과 버전만 입력한다. recipe variant에 따라 conda build string, BioContainer 이미지 후보 등 결정이 필요한 artifact가 다르다. Dockerfile fallback(사용자 직접 작성)과 source build(checksum 고정)를 제외한 4개 variant가 대상이다. NodeVault `ResolveRecipe` RPC가 Harbor 우선 조회로 후보를 반환한다. 이 RPC는 raw spec rewrite가 아니며, 최종 제출물의 package full pin 검증은 `SubmitToolBuild`/`BuildAndRegister` build gate가 담당한다.

| 항목 | 내용 | 우선순위 |
|------|------|---------|
| proto: `ResolveRecipe` RPC 추가 | `ResolveRecipeRequest` / `ResolveRecipeResponse` / `PackageResolution` / `BuildStringCandidate` 메시지 정의 | P2 |
| NodeVault Harbor 조회 | Harbor에서 동일 tool+version 이미지 탐색 → 이미지 메타데이터에서 artifact 정보 추출 → 후보 1개 반환 | P2 |
| 열린망 외부 소스 fallback (conda/micromamba/mirror) | Harbor 미존재 + 열린망 → conda 채널 repodata 조회 → build string 후보 목록 반환 | P2 |
| 열린망 외부 소스 fallback (BioContainer) | Harbor 미존재 + 열린망 → BioContainers registry 조회 → 이미지 후보 반환 | P3 |
| 폐쇄망 에러 응답 | Harbor 미존재 + 폐쇄망 → `InvalidArgument` ("Harbor 사전 등록 필요") | P2 |
| NodeKit UX — 후보 표시·선택 | NodeVault 반환 candidates를 NodeKit이 목록으로 표시 → 사용자 선택 → BuildRequest 생성 | P2 |
| NodeKit L1 완화 ✓ | `PackageVersionValidator`: `=version=build` 강제 → `=version` 형식만 요구 (구현 완료) | ✓ |

**완료 판정**

- [x] proto: `ResolveRecipe` RPC, `PackageResolution`, `BuildStringCandidate` 메시지 추가 (2026-06-28)
- [x] `bwa=0.7.17` 입력 → Harbor 캐시 명중 시 build string 1개 반환 (2026-06-28)
- [x] Harbor 미존재 + 열린망 → conda 채널에서 build string 후보 목록 반환 (2026-06-28)
- [x] Harbor 미존재 + 폐쇄망 → `InvalidArgument` 반환 (2026-06-28)
- [x] candidates 복수 시 NodeKit이 목록 표시 → 사용자 선택 → BuildRequest 고정 확인 — `PackageCandidatePresenter` + `RecipeCreateFlow` Step 7 구현 완료 (2026-07-02)
- [x] NodeKit `PackageVersionValidator` 테스트: `=version` 통과, 버전 미고정 거부 ✓

---

## 전체 우선순위 요약

```
완료 (2026-06-22)
  ├── Phase 1: ToolSpecRequest / ResolvedToolSpec 경계 적용
  └── Phase 3: Rootless UserNS seoy 클러스터 검증 (음성 경로 1건 제외)

완료 (2026-06-23)
  └── Phase 2: durable build state + cancel/timeout

완료 (2026-06-27)
  ├── Phase 3 잔여: rootless 실패 시 fallback 없음 확인 (TestBuildAndRegister_RootlessFailure_NoPrivilegedFallback)
  └── 트랙 A: OCI referrer (toolspec + toolprofile referrer, retention 포함 구현 완료)

완료 (2026-06-28)
  ├── 트랙 D: ResolveRecipe NodeVault 측 완료; NodeKit UX 진행 중
  ├── Phase 4: 캐시 계층 (package cache PVC + Harbor layer cache; seoy 라이브 검증 미완)
  └── Phase 5: Record/Certification 통합 — pkg/profiler hash·classifier 구현 완료

완료 (2026-06-29)
  ├── proto: BuildKind enum + BuildRequest 재설계 (ToolSpec/ToolFunctionSpec 분리)
  └── Phase 4: cache GC — pkg/cachegc high watermark 기반 eviction 구현 완료

완료 (2026-06-30)
  └── Phase 4: cache GC 버그 수정 (watermark≤0 가드, sub-MiB truncation)

완료 (2026-07-02)
  ├── 트랙 D: ResolveRecipe NodeKit UX 확인 완료 (PackageCandidatePresenter + RecipeCreateFlow Step 7)
  ├── fix(build): postBuildRegistration shared helper — SubmitToolBuild 경로 RegisterTool 누락 수정 (issue #10)
  ├── 트랙 C: ValidateService RBAC 이관 — BuildAndRegister L3/L4 직접 Job 생성 제거, 02-rbac.yaml 정리 (issue #11)
  └── deploy: kube-linter 6개 오류 해소 + CI hard gate 적용

NodeVault 남은 작업
  ├── Phase 4: seoy LayerCacheHit 라이브 검증 (seoy 필요, P3, issue #13)
  ├── TODO-16b: stableRef 재사용 UI 정책 합의 (NodeKit 조율 필요, issue #6)
  ├── 트랙 C: DagEdit ↔ NodePalette 연결 — NodeVault `/v1/palette/tools` alias 완료, DagEdit 소비자 연결은 외부 후속 (issue #14)
  ├── Phase 6: Legacy API 축소 (NodeKit 전환 완료 후)
  └── 재현성 개선 Sprint 5~11 (P0~P3) — Sprint 5·6·7·9(P0 RegistryConfig 통합, reconcile HTTPS/401, P1a build_state 브릿지, P2a 정적 risky-tool 정책) 완료, Sprint 8은 지금 당장 소비자가 없어 보류, Sprint 10은 podbridge5 issue #2 선행 필요, Sprint 11 loose mode는 NodeKit 합의 필요
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
| 7 | TODO-16b: stableRef 재사용 UI 정책 (Catalog UI revision 표시, active 전환 수동/자동) — NodeKit과 조율 필요, `docs/NONGOALS.md` N-07을 막고 있음 | NodeKit과 합의 시 — issue [#6](https://github.com/HeaInSeo/NodeVault/issues/6) |

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
