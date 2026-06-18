# NodeVault Proto Ownership Sprint Plan

작성일: 2026-04-18  
상태: sprint 4 complete

관련 문서:

- [PLATFORM_SCHEDULE.md](PLATFORM_SCHEDULE.md)
- [AUTHORITY_MAP.md](AUTHORITY_MAP.md)
- [DEPLOY_IN_CLUSTER.md](DEPLOY_IN_CLUSTER.md)

## 목적

이 문서는 `NodeVault` 가 자기 proto source ownership을 회수하고, 외부 `api-protos` 경로와 workspace 결합을 제거하기 위한 스프린트 일정을 고정한다.

현재 기준선:

- `nodeforge.proto` 는 이미 `NodeVault` 서비스 구현과 in-cluster gRPC/GRPCRoute 노출에 직접 연결돼 있다.
- `NodeVault` 빌드 체인은 `api-protos/gen/go/nodeforge/v1` 경로에 강하게 묶여 있다.
- proto ownership 정리와 build/vendor/workspace 정리는 한 번에 끝내지 않고 스프린트로 나눈다.

## Ownership Scope

이번 계획에서 `NodeVault` 가 회수할 대상:

- `nodeforge/v1/nodeforge.proto`
- `tool/ichthys/v1/tool_service.proto`
- `volres/ichthys/v1/volres_service.proto`

이번 계획에서 하지 않는 것:

- NodeVault 전체 범위 재설계
- Gateway API / GRPCRoute 구조 재설계
- build, validate, registry, catalog 전면 기능 변경
- Cilium mesh 정책 세부 확정

## Coordination Notice

`NodeVault` 쪽 proto ownership 정리 작업이 진행되는 동안 아래 범위는 임의 변경을 피한다.

- `api-protos/protos/nodeforge/v1/nodeforge.proto`
- `api-protos/protos/tool/ichthys/v1/tool_service.proto`
- `api-protos/protos/volres/ichthys/v1/volres_service.proto`
- `NodeVault` 의 proto import path
- `NodeVault/go.work`, Dockerfile, vendor 절차 중 proto 경로 관련 부분

이 범위는 migration 충돌 위험이 높으므로, 변경이 필요하면 먼저 ownership 일정과 충돌 여부를 확인한다.

## Sprint 0

기간 목표: 변경 동결과 ownership 선언

할 일:

- `NodeVault` 버킷 proto 를 문서상 확정
- 현재 외부 `api-protos` 결합 지점을 목록화
- 다른 진행 중 작업과 충돌 가능성이 큰 경로를 명시
- local canonical proto 위치 후보를 정한다

완료 기준:

- `NodeVault` 가 회수할 proto 범위가 문서상 분명하다
- 외부 `api-protos` 결합 지점이 목록으로 남아 있다
- 동시 작업자에게 proto/build path freeze 범위가 전달됐다

## Sprint 1

기간 목표: source `.proto` 회수

현재 상태:

- canonical source 초안이 `NodeVault/protos/` 아래에 추가되었다
- active import path 는 아직 external generated path 를 유지한다
- `go.work`, Dockerfile, vendor 절차는 아직 외부 `api-protos` 경로를 본다

할 일:

- `nodeforge.proto` 를 `NodeVault` 저장소 안 canonical 위치로 이동
- `tool_service.proto` 를 `NodeVault` 버킷으로 이동
- `volres_service.proto` 를 `NodeVault` 버킷으로 이동
- 새 canonical source 기준으로 code generation entry point 를 정의

완료 기준:

- `NodeVault` 저장소 안에 source `.proto` canonical 위치가 생긴다
- 회수 대상 3개 proto 가 더 이상 `api-protos` canonical source 로 읽히지 않는다
- generation 흐름을 `NodeVault` 내부에서 시작할 수 있다

Sprint 1 canonical paths:

- `NodeVault/protos/nodeforge/v1/nodeforge.proto`
- `NodeVault/protos/tool/ichthys/v1/tool_service.proto`
- `NodeVault/protos/volres/ichthys/v1/volres_service.proto`

Sprint 1 hold line:

- import path 전환은 Sprint 2로 넘긴다
- `go.work`, Dockerfile, vendor 정리는 Sprint 3로 넘긴다

## Sprint 2

기간 목표: code generation 과 import 경로 전환

현재 상태:

- local source 기준 `nodeforge` generated code 가 `NodeVault/protos/nodeforge/v1` 아래에 생성되었다
- active code import 는 local generated path 기준으로 전환되었다
- `go.work`, Dockerfile, vendor 절차는 아직 외부 `api-protos` 경로를 유지한다

할 일:

- local source 기반 generated code 생성
- `NodeVault` 코드 import 를 새 local generated path 로 전환
- test/build 에서 새 generated path 사용 확인
- `tool`, `volres` 가 아직 구현 연결이 약한 경우에도 source ownership 기준만 먼저 고정

완료 기준:

- active `NodeVault` 코드가 `api-protos/gen/go/nodeforge/v1` 를 요구하지 않는다
- source `.proto` 와 generated path 의 owner 가 모두 `NodeVault` 가 된다
- `tool`, `volres` 도 더 이상 `api-protos` canonical source 아래에 남지 않는다

## Sprint 3

기간 목표: workspace, Docker, vendor 결합 제거

현재 상태:

- `go.work` 는 제거되었고 workspace 기반 외부 proto 결합은 더 이상 없다
- Dockerfile 은 외부 `api-protos` 복사와 sed 치환 없이 local source 기준으로 빌드된다
- vendor 절차는 `go mod vendor` 기준으로 단순화되었다

할 일:

- `go.work` 에서 외부 `api-protos` use 경로 제거
- Dockerfile 의 `api-protos` 복사와 sed 치환 제거
- vendor 절차를 local proto/generation 기준으로 정리
- `make vendor`, 이미지 빌드, in-cluster build 경로를 재검증

완료 기준:

- workspace 기반 외부 `api-protos` 참조가 없다
- Docker build 가 `api-protos` rsync/COPY 없이 동작한다
- build/vendor 문서가 새 경로 기준으로 정리된다

## Sprint 4

기간 목표: `api-protos` 제거 준비 완료 선언

현재 상태:

- active code, build, test 경로에서 외부 `api-protos` import 는 제거되었다
- `go mod tidy`, `go mod vendor`, tagged `go test ./...` 검증이 workspace 제거 이후 기준으로 다시 통과했다
- `NodeVault` 는 `api-protos` 제거 시 추가 조치 없이 자기 proto source 와 generated path 로 동작한다

할 일:

- `NodeVault` 의 active build/test/deploy 경로에 `api-protos` 의존이 없는지 최종 확인
- deployment 문서와 proto ownership 문서를 새 기준으로 정리
- `api-protos` sunset plan exit criteria 중 `NodeVault` 책임 항목 완료 확인

완료 기준:

- `NodeVault` 는 자기 source `.proto` 를 직접 소유한다
- `NodeVault` build/test/deploy 경로가 `api-protos` 를 요구하지 않는다
- `api-protos` 제거 시 `NodeVault` 쪽 blocker 가 없다

최종 상태:

- `NodeVault/protos/` 가 canonical source 다
- `NodeVault/protos/nodeforge/v1` generated path 가 active import 경로다
- `go.work` 없이 `go mod` 와 `vendor` 만으로 빌드/테스트가 통과한다
- `api-protos` 는 문서상 sunset 대상일 뿐, `NodeVault` 의 active dependency 가 아니다

## Recommended Order Inside NodeVault

1. source `.proto` 회수
2. generated code 생성 기준 고정
3. import path 전환
4. `go.work` 정리
5. Docker/vendor 정리
6. deploy 문서 정리

## Risks

- 현재 다른 작업자가 `NodeVault` proto/build 경로를 동시에 수정하면 migration 충돌이 날 수 있다
- `go.work` 와 Dockerfile 이 외부 `api-protos` 경로에 묶여 있어 빌드 회귀 가능성이 크다
- `tool` 과 `volres` 는 구현 연결이 약해 ownership 회수 후에도 임시 상태가 길어질 수 있다

## Hold Line

스프린트 진행 중에도 아래는 유지한다.

- `NodeVault` runtime 기능 확장보다 proto ownership 회수를 우선
- `api-protos` 는 새로운 canonical source 로 사용하지 않음
- build path 정리 전 임의 경로 변경 금지
- north-south gRPC 노출 구조는 proto ownership 회수와 분리해서 다룸
