# NodeVault 비목표 목록 (TODO-03)

작성일: 2026-04-14  
상태: **확정** — PR 리뷰 체크포인트로 작동

이 목록은 현재 버전 범위에서 **의도적으로 구현하지 않는 항목**이다.
PR 리뷰 시 아래 항목이 포함되어 있으면 즉시 차단한다.

---

## 비목표 항목

### N-01: Harbor 외부 레지스트리 지원
- 현재 범위: 내부 레지스트리(registry:2, NodePort 31500) 또는 Harbor(설치 후)
- 비목표: DockerHub, ECR, GCR, Quay 등 외부 레지스트리 직접 연동
- 이유: 재현성 정책(same data + same method = same result)은 플랫폼 통제 레지스트리에서만 보장 가능

### N-02: 다중 클러스터 지원
- 비목표: NodeVault가 여러 K8s 클러스터를 동시에 관리
- 이유: 현재 단일 kubeconfig + 단일 클러스터 범위. 멀티클러스터는 별도 로드맵.

### N-03: artifact 자동 삭제 (reconcile loop에서)
- 비목표: `integrity_health = Orphaned` 상태에서 자동 삭제 실행
- 이유: Orphaned 기본 정책 = 수동 대기 + 알람. 자동 삭제는 운영 사고 위험.
- 삭제는 `lifecycle_phase = Deleted` 전이 + Harbor GC 순서로 명시적 실행.

### N-04: DagEdit / 파이프라인 편집기 연동
- 비목표: NodeVault 또는 NodeKit이 DagEdit 내부 구조를 알거나 결합
- 이유: DagEdit는 별도 프로젝트 트랙. NodeVault는 Catalog REST API만 노출.

### N-05: `latest` 태그 허용 모드
- 비목표: `latest` 허용 bypass 플래그, 설정, 또는 "allow-latest" 옵션
- 이유: 재현성 비타협 원칙. `latest`는 L1에서 무조건 차단.

### N-06: 파이프라인 실행 엔진
- 비목표: NodeVault가 파이프라인을 직접 실행하거나 스케줄링
- 이유: NodeVault는 artifact 등록·관리만 담당. 파이프라인 실행은 별도 엔진.

### N-07: stableRef current active 단수 포인터 (현재)
- 비목표: `stableRef → current active casHash` 매핑 레코드를 index에 지금 추가
- 이유: TODO-16b(UI revision 정책)가 결정되기 전까지 설계 확정 불가.

### N-08: TODO-09b NodeVault 런타임 전환 (인프라 안정화 전)
- 비목표: Cilium + Harbor 안정화 전에 NodeVault → NodeVault 런타임 전환 구현
- 이유: 09a(설계)와 09b(구현) 동시 시작 시 인프라/semantic/배포 실패 원인 분리 불가.

### N-09: data artifact 등록 (현재)
- 비목표: `DataDefinition` / `DataRegisterRequest` 구현 (P3까지 유예)
- 이유: index 스키마(TODO-06)에 data 자리는 지금 확보하되, 구현은 P3.

### N-10: Harbor webhook-first 아키텍처
- 비목표: webhook을 primary consistency 경로로 사용
- 이유: Harbor GC 이벤트는 webhook 표면에 없음. reconcile-first가 기본.

### N-11: NodeVault가 패키지/이미지 후보를 대신 선택 (issue #18)
- 비목표: `ResolveRecipe`가 여러 candidate 중 하나를 "정답"으로 골라 반환하거나,
  NodeVault가 후보 선택 결과를 받아 발급하는 별도 canonical candidate API
- NodeVault가 계속 담당하는 것: `ResolveRecipe`의 candidate lookup, 제출된
  ToolSpec의 정규화와 content digest 계산(`ResolveToolSpec`), submit 시점의
  완전한 pin(`name=version=build`) 및 정책 검증(`SubmitToolBuild` 최종 게이트)
- 이유: 어떤 package build/이미지를 쓸지는 CPU 호환성, 기존 파이프라인 호환,
  관리자 정책, 재현 대상 연구 결과 등 authoring 의도가 섞인 판단이라 NodeKit/
  사용자 책임이다. NodeVault가 대신 고르면 CLAUDE.md §1의 authoring UX 경계를
  침범한다. 재현성의 최소 요구(완전한 pin)는 이미 Submit 게이트가 강제한다.

---

## PR 리뷰 체크포인트

PR 리뷰 시 다음 질문으로 비목표 포함 여부를 확인한다:

1. 외부 레지스트리(DockerHub/ECR/GCR) 연동 코드가 있는가? → **N-01 차단**
2. Orphaned 상태에서 artifact를 자동 삭제하는 코드가 있는가? → **N-03 차단**
3. DagEdit 내부 타입/인터페이스를 import하는가? → **N-04 차단**
4. `latest` 태그 허용 경로(bypass, 설정 등)가 있는가? → **N-05 차단**
5. Cilium/Harbor 안정화 전에 NodeVault 런타임 분리를 구현하는가? → **N-08 차단**
6. DataDefinition/DataRegisterRequest를 P1/P2에서 구현하는가? → **N-09 차단**
7. reconcile loop가 `lifecycle_phase`를 변경하는가? → **비목표 + CLAUDE.md §12 차단**
8. `integrity_health`를 Catalog 노출 조건으로 사용하는가? → **비목표 + CLAUDE.md §12 차단**
9. NodeVault가 candidate 중 하나를 선택해 canonical spec/이미지로 발급하는 코드가 있는가? → **N-11 차단**
