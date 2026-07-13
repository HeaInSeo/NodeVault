# podbridge5

`podbridge5`는 컨테이너 기반 실행 환경을 다루는 Go 라이브러리입니다.
이미지 빌드, 이미지 push, 컨테이너 생성과 실행, 볼륨 데이터 주입 같은 작업을 하나의 코드베이스에서 다룰 수 있게 하는 것이 목적입니다.

이 프로젝트는 Kubernetes 전용 라이브러리가 아닙니다.
Kubernetes 위에서 사용하는 상위 프로젝트가 있을 수는 있지만, `podbridge5` 자체는 더 일반적인 컨테이너 런타임 유틸리티 계층으로 설계하고 있습니다.

## 주요 역할

- 컨테이너 생성, 시작, 상태 확인
- 컨테이너 healthcheck 계약 관리
- 이미지 빌드와 push 보조
- named volume 생성, 교체, 재사용
- 호스트 디렉터리와 volume 사이 데이터 복사
- runtime test를 위한 재현 가능한 실행 경로 제공

## 어디에 쓸 수 있나

- 컨테이너 실행 기반 도구 백엔드
- 빌드/배포 파이프라인의 runtime helper
- 개발용 container sandbox
- 상위 orchestration 계층 아래의 runtime adapter
- Kubernetes 외 환경의 container automation

## 프로젝트 방향

현재 이 저장소에서 중요하게 보는 것은 두 가지입니다.

1. runtime 의존 코드와 순수 로직을 분리하는 것
2. clean VM 기준으로 재현 가능한 테스트 경로를 유지하는 것

즉, 문제를 만났을 때
- 순수 로직 문제인지
- 실제 Podman/Buildah/storage 초기화 문제인지
를 빠르게 가를 수 있어야 합니다.

## Go 기준선

이 저장소는 `go.mod` 기준으로 `Go 1.25.6`을 사용합니다.
다른 관련 프로젝트들과 동일한 `Go 1.25.x` 기준선을 유지하는 것이 정책입니다.

- 로컬 테스트와 개발 진입점은 `make test-unit`, `make test-runtime`, `make test-runtime-integration`입니다.
- 이 타깃들은 실행 전에 현재 `go` 바이너리가 `go1.25.6`인지 확인합니다.
- 낮은 버전과의 호환성을 목표로 하지 않습니다. 버전을 낮추는 대신 개발 환경과 CI/VM을 `1.25.x`에 맞춥니다.

## Kubernetes user namespace 빌드 준비

NodeVault의 rootless builder 전환을 위해 `podbridge5`는 Buildah user namespace용 기본 옵션을 제공합니다.
`DefaultUserNamespaceStoreOptions()`는 별도 `storage.conf` 파일 없이도 `/storage` 아래의 `runroot`, `graphroot`, overlay driver, partial image pull 기본값을 코드에서 구성합니다.
필요하면 `WithStoreRoots`, `WithStoreDriver`, `WithFuseOverlayfsMountProgram`, `WithPartialImagePulls`로 런타임 환경에 맞게 조정합니다.

빌드 옵션은 `UserNamespaceImageBuildOptions()`와 `BuildDockerfileContentUserNamespace()`를 사용합니다.
새 build-and-push 호출은 `NewUserNamespaceBuildConfig()`로 시작하면 OCI isolation과 storage mode가 명시됩니다. 기존 `UserNamespaceBuildConfig{}`의 빈 `Isolation`은 호환성을 위해 `chroot`로 해석되므로, 공개 소비자는 isolation을 명시해야 합니다. Harbor 캐시 저장소는 `CacheRef`로 `cache-from/cache-to`에 동시에 연결합니다.
`DefaultUserNamespaceBuildEnvironment()`와 `DefaultUserNamespaceBuildCapabilities()`는 예시일 뿐입니다. capability, AppArmor, storage driver는 배포 런타임별로 검증해야 하며 라이브러리가 특정 Kubernetes 보안 정책을 보장하지 않습니다.

### Persistent Multipass runtime VM

원격 Buildah 런타임 검증용 `podbridge5-dev` VM은 [infra/multipass](infra/multipass/README.md)의 HCL로 관리합니다. 이 VM은 pd5 전용 검증 host이므로 `infra-lab` Kubernetes 인프라와 분리되어 있습니다.

```bash
cd infra/multipass
tofu init && tofu apply

cd ../..
make vm-prepare-runtime REMOTE_USER=seoy
make vm-sync-runtime REMOTE_USER=seoy
make vm-run-runtime REMOTE_USER=seoy
```

`make vm-test-runtime`은 호환성 경로이며 테스트 뒤 VM을 삭제합니다. HCL로 관리하는 persistent VM에는 위의 prepare/sync/run 순서를 사용합니다.
overlay는 목표 기본값으로 유지하되, NodeVault 전환 시 node filesystem/idmap 또는 fuse-overlayfs 경로를 별도 검증해야 합니다.

이 경로는 Kubernetes 1.36 계열 user namespace, Linux 6.3 이상 수준의 idmapped mount 지원, containerd 2.x, crun 1.9+ 조합을 전제로 합니다.
호환되지 않는 Dockerfile은 이후 NodeVault의 privileged isolated fallback 전략으로 분리합니다.

## 변경 이력

- `v0.1.8` — `SaveImage`/`ImageArchivePath` 공개 API 추가(`saveImage`/`imageArchivePath`의 얇은 공개 래퍼). 빌드된 이미지를 tar/tar.gz로 export해 파일 목록을 exec 없이 검사할 수 있게 함(issue #2, NodeVault의 최종 이미지 risky-tool 스캔용)
- `v0.1.7` — lint 수정(importShadow, nil context), `NewConfigFromFile`·`GenerateExecutor`·`ProcessScript` 단위 테스트 보강. `RunContainerWithStats` 삭제(삭제 예정 코드). `BuildConfig.CreateImage()` 시그니처 변경: 전역 상태(`pbCtx`/`pbStore`) 의존 제거, `(ctx context.Context, store storage.Store)` 명시 파라미터로 전환(`CreateImageWithDockerfile`과 동일한 패턴). TODO 주석 정리, GitHub Actions VM 런타임 테스트 워크플로 SSH 시크릿 설정 개선
- `v0.1.6` — `NewUserNamespaceBuildConfig()` 생성자와 `ClassifyBuildahExecutionError()` 분리 추가, `UserNamespaceBuildConfig`에 `NetworkConfiguration` 옵션 추가. 원격 검증 VM에서 발견한 버그 수정: rootless netavark가 OCI isolation 빌드의 `RUN` 단계에서 네트워크 네임스페이스를 설정하지 못하는 문제(`setns: Operation not permitted`)를 `NetworkConfiguration: NetworkDisabled`로 우회. `infra/multipass`에 persistent 검증 VM(`podbridge5-dev`) 관리용 OpenTofu 모듈 추가
- `v0.1.5` — 실제 런타임/통합 테스트 커버리지 확대(store, builder, registry push/pull, user namespace 빌드, rootless init 경로). 이 과정에서 발견한 버그 수정: `runc` 하드코딩 제거(VM은 `crun`만 설치됨), `RemoveIntermediateCtrs` 미설정으로 인한 빌드 레이어 누수, 불필요한 netavark 네트워크 설정, 잘못된 `Chmod` 기본값(`"0o755"` → `"755"`), 볼륨 정리 타임아웃 조정. remote VM 통합 테스트 하니스를 `unshare -r`에서 `podman unshare`로 교체(subuid/subgid 전체 범위 및 rootless 환경변수를 올바르게 적용), 합산 테스트 커버리지 80%+ 달성
- `v0.1.4` — `MountOverlay`의 fuse-overlayfs 폴백 제거(네이티브 rootless overlay만 시도), `CreateContainer`의 create-if-absent 계약 명시 및 관련 테스트 정리, remote VM 런타임 테스트 파이프라인 수정(소켓 권한, 출력 스트리밍 끊김), 합산 테스트 커버리지 70%+ 달성
- `v0.1.2` — Podman/Buildah 의존성 업데이트, remote VM user namespace 통합 수정, user namespace Buildah 기본값 추가
- `v0.1.1` — `seoyhaein/utils`를 `HeaInSeo/utils v0.0.7`로 교체 (API 변경 없음)

## 문서

- 한국어 runtime 검증 문서: [docs/runtime-testing.ko.md](docs/runtime-testing.ko.md)
- 한국어 runtime 초기화 정책: [docs/runtime-policy.ko.md](docs/runtime-policy.ko.md)
- 한국어 dry-run / timeout 정책: [docs/execution-policy.ko.md](docs/execution-policy.ko.md)
- 한국어 구현 우선순위: [docs/implementation-priorities.ko.md](docs/implementation-priorities.ko.md)
- English overview: [README.en.md](README.en.md)
- English runtime validation: [docs/runtime-testing.en.md](docs/runtime-testing.en.md)
- English runtime policy: [docs/runtime-policy.en.md](docs/runtime-policy.en.md)
- English dry-run / timeout policy: [docs/execution-policy.en.md](docs/execution-policy.en.md)
- 현재 리팩터 스프린트 문서: [docs/sprint-2026-04-14-runtime-refactor.md](docs/sprint-2026-04-14-runtime-refactor.md)
- 프로젝트 상태 및 스프린트 로드맵: [docs/project-status-roadmap.ko.md](docs/project-status-roadmap.ko.md)
- 기존 README 초안 백업: [backup/Readme.legacy.md](backup/Readme.legacy.md)

## 현재 검증 방식

일반적인 빠른 확인은 로컬에서 하고, runtime 의존 검증은 remote Multipass VM에서 수행합니다.
자세한 절차와 Makefile 타깃은 runtime 검증 문서로 분리했습니다.
