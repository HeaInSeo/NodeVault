# NodePalette 서비스 설계

작성일: 2026-04-19
상태: 설계 확정 (구현 대기 — TODO-10)

---

## 이름 결정 근거

`NodeCatalog`는 도구만 포함하는 것처럼 읽힌다.
NodePalette는 tools + reference data 양쪽을 파이프라인 빌더에게 제공하는 read-only
artifact palette다. CLAUDE.md §1의 "Read-only artifact palette (tools + reference data)"
정의와 직접 일치한다.

---

## 역할 한 줄 정의

`lifecycle_phase = Active`인 tool + data artifact를 파이프라인 빌더(DagEdit 등)와
NodeKit AdminToolList/AdminDataList에 제공하는 **read-only REST 서비스**.

---

## 이름 및 배포 결정 (확정)

| 항목 | 결정값 |
|------|--------|
| 서비스 이름 | `NodePalette` |
| 바이너리 | `nodepalette` |
| GitHub 저장소 | NodeVault 안 `cmd/palette/` (같은 repo, 별도 진입점) |
| K8s namespace | `nodepalette-system` |
| K8s Deployment | `nodepalette` |
| GRPCRoute/hostname | `palette.10.113.24.96.nip.io` |
| 포트 | HTTP `:8080` (현재 NodeVault 내 Catalog REST와 동일) |

---

## 저장소 전략: 같은 repo, 별도 바이너리

별도 repo를 만들지 않고 NodeVault repo 안에 `cmd/palette/` 진입점을 추가한다.

**이유:**
- `pkg/index`, `pkg/catalog`, `protos/nodeforge/v1` 타입을 직접 공유 — cross-repo 의존성 없음
- storage(`assets/`) 를 같은 PVC로 마운트, 별도 내부 API 불필요
- go.mod 하나, 배포 파이프라인 통합 유지
- 향후 책임이 커지면 별도 repo 이전 가능

---

## 소스 구조 (TODO-10 구현 시)

```
NodeVault/
  cmd/
    controlplane/   ← NodeVault 바이너리 (기존)
    palette/        ← NodePalette 바이너리 (신규)
      main.go       ← pkg/catalogrest 진입점
  pkg/
    catalogrest/    ← NodePalette가 사용하는 read-only REST 핸들러
    index/          ← NodeVault + NodePalette 공유
    catalog/        ← NodeVault + NodePalette 공유 (read 경로만)
```

`cmd/controlplane/main.go`에서 현재 인라인으로 실행 중인 `catalogrest` HTTP 서버를
`cmd/palette/main.go`로 이전한다.

---

## 책임 경계

| 항목 | NodeVault | NodePalette |
|------|-----------|-------------|
| artifact 등록 (write) | O | X |
| lifecycle_phase 변경 | O | X |
| integrity_health 변경 | O (reconcile 경유) | X |
| tool 목록 조회 | X | O |
| data 목록 조회 | X | O |
| artifact 단건 조회 | X | O |

NodePalette는 `index.Store`와 `catalog.Catalog`를 **읽기 전용**으로만 접근한다.
NodeVault가 index를 변경하면 NodePalette는 다음 요청 시 반영된 값을 읽는다.

---

## API 엔드포인트 (현행 pkg/catalogrest 그대로)

| 엔드포인트 | 설명 |
|------------|------|
| `GET /v1/catalog/tools` | lifecycle_phase=Active tool 목록 |
| `GET /v1/catalog/tools/{cas_hash}` | tool 단건 조회 |
| `GET /v1/catalog/data` | lifecycle_phase=Active data 목록 |
| `GET /v1/catalog/data/{cas_hash}` | data 단건 조회 |
| `GET /v1/palette/tools` | `GET /v1/catalog/tools` alias. DagEdit/NodePalette 소비자용, 응답에 `cas_hash` 포함 |
| `GET /v1/palette/data` | `GET /v1/catalog/data` alias. 응답에 `cas_hash` 포함 |

노출 기준: `lifecycle_phase = Active`만. `integrity_health`는 무관.

---

## 현재 상태 (NodeVault 바이너리 내 인라인)

NodePalette 분리 전까지 `pkg/catalogrest`는 NodeVault 바이너리 안에서 `:8080`으로
인라인 실행된다. NodeKit `HttpCatalogClient`는 이미 이 엔드포인트를 사용하고 있다.

분리 후에도 API 경로와 응답 형식은 동일하게 유지된다 — NodeKit 변경 불필요.

---

## 구현 선행 조건

TODO-10 (선행: TODO-09b — Cilium + Harbor 안정화) 참고.

분리 자체는 단순 진입점 추가 + main.go에서 catalogrest goroutine 제거이므로
인프라 준비 완료 후 빠르게 진행 가능.
