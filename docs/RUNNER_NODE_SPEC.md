# Runner Node Spec — DagEdit RunnerNode Contract

버전: v0.1.0-draft
작성일: 2026-05-03
상태: Sprint 0 (DagEdit 설계 선행 계약 — DagEdit repo에서 구현)
근거: `NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` §3.3

---

## 1. 원칙

### 1.1 재현성

- `casHash`는 **필수**. 실행 pin. 파이프라인 저장·실행의 유일한 식별자.
- `stableRef`는 UI 표시/검색용 **snapshot**. 실행에 사용하지 않는다.
- 같은 `casHash` → 같은 image digest → 같은 tool binary → 재현 가능한 실행.

### 1.2 stableRef 사용 금지 규칙

`stableRef`(`tool_name@version`)은 1:N 관계다 — 같은 stableRef로 여러 casHash가 존재할 수 있다 (재빌드, 이미지 갱신). **파이프라인 노드는 반드시 casHash로 pin**한다.

`stableRef`를 파이프라인 실행 식별자로 사용하면 동일한 파이프라인이 다른 결과를 낼 수 있다 (비재현성). 이는 L1 검증 위반과 동등한 재현성 파괴다.

---

## 2. RunnerNode JSON 구조

### 2.1 필수 필드

| 필드 | 타입 | 비고 |
|------|------|------|
| `nodeType` | string | `"runner"` 고정 |
| `casHash` | string | `sha256:...` 형식. 실행 pin. **필수** |

### 2.2 선택 필드

| 필드 | 타입 | 비고 |
|------|------|------|
| `stableRef` | string | `tool_name@version`. UI 표시용 snapshot. |
| `displaySnapshot` | object | 팔레트 표시용 label/category/icon |
| `portBindings` | object | DAG edge 연결 정보 |
| `portMetadata` | object | UI 호환성 판단 + badge용 |

### 2.3 MVP 구조 예시

```json
{
  "nodeType": "runner",
  "casHash": "sha256:abc123...",
  "stableRef": "bwa@0.7.17",
  "displaySnapshot": {
    "label": "BWA 0.7.17",
    "category": "Alignment",
    "icon": null
  },
  "portBindings": {
    "inputs": {
      "reads": {
        "connectedTo": "fastq-source:output"
      },
      "reference": {
        "connectedTo": "ref-node:output"
      }
    },
    "outputs": {
      "alignment": {}
    }
  },
  "portMetadata": {
    "source": "declared+observed",
    "toolspecReferrerDigest": "sha256:toolspec...",
    "observedProfileDigest": "sha256:profile...",
    "validationHash": "sha256:vhash..."
  }
}
```

---

## 3. portBindings vs portMetadata 분리

| 필드 | 역할 | 실행 필요 여부 |
|------|------|---------------|
| `portBindings` | 실제 DAG edge 연결 (어느 노드의 어느 출력이 이 노드의 입력에 연결됐나) | **필수** |
| `portMetadata` | UI 호환성 판단 + badge 표시 (observed profile/validation 증거) | 실행에 영향 없음 |

`portMetadata`는 DagEdit UI에서 포트 호환성 badge와 경고를 표시할 때 사용한다. 실행 엔진은 `portBindings`만 사용한다.

---

## 4. DagEdit → NodePalette 연결 흐름

```
DagEdit 팔레트에서 도구 선택
  │
  ▼
GET /api/v1/tools?stableRef=bwa@0.7.17
  (NodePalette REST — pkg/catalogrest)
  │
  ▼
응답: { casHash, stableRef, display, inputs, outputs, observedProfileDigest, ... }
  │
  ▼
RunnerNode 생성:
  casHash     = 응답.casHash         ← 실행 pin
  stableRef   = 응답.stableRef       ← UI snapshot
  portMetadata.observedProfileDigest = 응답.observedProfileDigest
  portMetadata.validationHash        = 응답.validationHash (있을 경우)
```

**중요**: stableRef로 도구를 검색하되, RunnerNode에 기록하는 식별자는 반드시 casHash다.

---

## 5. portMetadata 세부 필드

| 필드 | 출처 | 의미 |
|------|------|------|
| `source` | DagEdit 선택 시점 | metadata 출처 (`declared` / `declared+observed`) |
| `toolspecReferrerDigest` | NodePalette 응답 | toolspec OCI referrer digest |
| `observedProfileDigest` | NodePalette 응답 | toolprofile OCI referrer digest (있을 경우) |
| `validationHash` | NodePalette 응답 | 마지막 successful dry-run validation hash (있을 경우) |

`observedProfileDigest`와 `validationHash`는 모두 optional이다. 없으면 DagEdit은 "Unverified" badge를 표시한다.

---

## 6. UI Badge 정책

| 조건 | badge |
|------|-------|
| `observedProfileDigest` 없음 | Unverified |
| `validationHash` 없음 | No dry-run profile |
| 두 필드 모두 있음 | Verified |
| `portMetadata.source: "declared"` | Declared only |

badge는 표시 전용. DagEdit에서 연결 자체를 막지는 않는다 (선택 사항).

---

## 7. 구현 경로

| 항목 | 위치 | Sprint |
|------|------|--------|
| NodePalette REST 응답에 `observedProfileDigest`, `validationHash` 포함 | `pkg/catalogrest` | Sprint 1 |
| DagEdit RunnerNode JSON schema 정의 | DagEdit repo | P5 이후 |
| DagEdit 팔레트 → NodePalette 조회 연결 | DagEdit repo | P5 이후 |

현재 NodeVault 쪽 의존성: NodePalette가 `observedProfileDigest`와 `validationHash`를 응답에 포함하면 된다 (Sprint 1 이후). DagEdit 구현은 별도 repo에서 진행한다.

---

## 8. 관련 문서

- `TOOL_CONTRACT_V0_2.md` — `casHash`/`stableRef` 경계 규칙 (확정 계약)
- `TOOL_CONTRACT_V0_3_DRAFT.md` — `validationHash`, `observedProfileDigest` additive field 명세
- `OBSERVED_PROFILE_SPEC.md` — `validationHash` 생성 규칙
- `TOOL_NODE_SPEC.md` — 계층 5 (파이프라인 노드) 개요
- `NODEVAULT_V03_MAPPING.md` — 용어 ↔ 코드 위치 대응표
