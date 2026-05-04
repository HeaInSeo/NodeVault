# Security Scan Spec — `security` referrer

버전: v0.1.0-draft
작성일: 2026-05-03
상태: Sprint 0 (코드 변경 없음 — 스펙 선언)
근거: `NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` §5.2, §5.3, §6

---

## 1. artifactType

```
application/vnd.nodevault.security.v1+json
```

`toolspec` 및 `toolprofile` referrer와 독립 분리된다.

**분리 이유**: CVE DB와 scanner version에 따라 결과가 시간에 따라 변한다. `toolspec`(선언 시점 고정)과 `toolprofile`(functional validation 재실행 시 갱신)과 독립적으로 갱신되어야 한다. 한 artifact에 묶으면 CVE 재스캔만으로 `toolspec`/`toolprofile`까지 교체해야 하는 문제가 생긴다.

---

## 2. Payload 구조

```json
{
  "artifactType": "application/vnd.nodevault.security.v1+json",
  "subject": {
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": "sha256:IMAGE_DIGEST"
  },
  "security": {
    "scanner": "trivy",
    "scannerVersion": "0.50.0",
    "source": "trivy-operator",
    "reportKind": "VulnerabilityReport",
    "reportDigest": "sha256:VULNERABILITY_REPORT_DIGEST",
    "scanTime": "2026-05-03T00:00:00Z",
    "summary": {
      "critical": 0,
      "high": 2,
      "medium": 5,
      "low": 12,
      "unknown": 0
    },
    "misconfiguration": {
      "high": 0,
      "medium": 1,
      "low": 3
    },
    "secretExposure": {
      "count": 0
    },
    "policy": {
      "mode": "record_only",
      "result": "warning",
      "activeGate": false,
      "evaluatedAt": "2026-05-03T00:00:00Z"
    }
  }
}
```

### 2.1 필드 설명

| 필드 | 의미 |
|------|------|
| `scanner` | 사용한 scanner 이름 (`trivy`) |
| `scannerVersion` | scanner 버전 |
| `source` | scan 수행 주체 (`trivy-operator` / `direct`) |
| `reportKind` | 원본 보고서 유형 (`VulnerabilityReport`) |
| `reportDigest` | 전체 report artifact digest (상세 조회용) |
| `scanTime` | scan 수행 시각 (UTC RFC3339) |
| `summary` | CVE 심각도별 개수 |
| `misconfiguration` | 설정 오류 심각도별 개수 |
| `secretExposure` | 노출된 secret 개수 |
| `policy.mode` | 적용 정책 모드 (`record_only` / `gate_critical` / `gate_high`) |
| `policy.result` | 정책 평가 결과 (`pass` / `warning` / `blocked`) |
| `policy.activeGate` | Active 전환 차단 여부 |

---

## 3. trivy-operator 통합 방향

```
trivy-operator
  → nodevault-security namespace에서 동작
  → Harbor image scan 완료 시 VulnerabilityReport CR 생성

NodeVault reconcile loop (병렬 트랙, pkg/reconcile)
  → VulnerabilityReport CR 조회
  → CVE summary 추출 + reportDigest 계산
  → PushSecurityReferrer (pkg/oras — Sprint 병렬 트랙)
  → image에 security referrer attach
  → index.Entry.SecurityScanDigest 갱신
  → integrity_health 평가 갱신
```

### 3.1 현재 미구현 항목

- `PushSecurityReferrer` (`pkg/oras/referrer.go` — 병렬 트랙)
- `index.Entry.SecurityScanDigest` (`pkg/index/schema.go` — Sprint 1)
- trivy-operator K8s CR 조회 로직 (`pkg/reconcile` — 병렬 트랙)

---

## 4. 기본 정책: record_only

### 4.1 원칙

**기록은 기본이다. Gate는 정책 옵션이다.**

- 어떤 image를 언제 scan했는가 기록
- 어떤 scanner/version을 사용했는가 기록
- CVE 개수 기록
- 전체 report digest 기록
- scan 결과 freshness 기록

Active 전환 차단은 기본값 꺼짐. 조직 정책에 따라 활성화.

### 4.2 policy.mode 값

| mode | 의미 |
|------|------|
| `record_only` | scan 결과 기록만. Active 전환 차단 없음 (기본) |
| `gate_critical` | critical CVE 존재 시 Active 전환 차단 |
| `gate_high` | critical 또는 high CVE 존재 시 Active 전환 차단 |

### 4.3 integrity_health 반영

| 상태 | 조건 |
|------|------|
| `Healthy` | image ✓, toolspec referrer ✓, security scan 있고 policy pass |
| `Partial` | image ✓, toolspec referrer ✓, security scan 없음 |
| `Warning` | security scan 있음 + critical/high CVE 존재 (gate 꺼진 경우) |
| `Blocked` | `gate_critical`/`gate_high` 정책이 켜져 있고 위반됨 |

`integrity_health`는 모니터링/알람 전용. Catalog 노출은 `lifecycle_phase = Active`만으로 결정한다.

---

## 5. Scan Freshness

scan 결과의 신선도를 관리한다. CVE DB는 지속적으로 갱신되므로 오래된 scan 결과는 신뢰도가 낮아진다.

| 정책 | 기본값 |
|------|--------|
| scan 결과 유효 기간 | 30일 |
| 만료 시 `integrity_health` | `Partial` (scan 없음과 동일하게 취급) |
| 만료 시 재스캔 트리거 | reconcile loop에서 감지 |

---

## 6. Referrer Retention / GC

### 6.1 index.Entry 캐시

`pkg/index/schema.go:Entry`에 `SecurityScanDigest` 필드를 추가한다 (Sprint 1).

```go
SecurityScanDigest string `json:"security_scan_digest,omitempty"`
```

- latest security referrer digest 1개만 캐시
- 갱신 주체: reconcile loop (병렬 트랙)

### 6.2 Registry 보존

| 정책 | 값 |
|------|-----|
| 유지 개수 | latest 3개 |
| 유지 기간 | 최근 30일 |
| 초과분 처리 | GC candidate 표시 (즉시 삭제 안 함) |
| 삭제 | registry GC 정책에 위임 |

---

## 7. NodePalette UI Badge

| 상태 | badge |
|------|-------|
| `securityScanDigest` 없음 | Security Not Scanned |
| scan 있음 + `policy.result: "pass"` | Security Pass |
| scan 있음 + `policy.result: "warning"` | Security Warning |
| scan 있음 + `policy.activeGate: true` + 위반 | Security Blocked |

**기본 목록**: security scan 없는 tool도 포함. `scanned only` filter 선택 시 제외.

---

## 8. 구현 경로

| 단계 | 내용 | Sprint |
|------|------|--------|
| `pkg/index/schema.go` | `SecurityScanDigest` 필드 추가 | Sprint 1 |
| `pkg/oras/referrer.go` | `PushSecurityReferrer` 추가 | 병렬 트랙 |
| `pkg/reconcile` | VulnerabilityReport CR 조회 + push hook | 병렬 트랙 |
| NodePalette `pkg/catalogrest` | security badge 응답 포함 | 병렬 트랙 |

---

## 9. 관련 문서

- `TOOL_CONTRACT_V0_3_DRAFT.md` — 상위 계약 (`securityScanDigest` field 명세)
- `OBSERVED_PROFILE_SPEC.md` — `toolprofile` referrer (functional validation, 독립)
- `NODEVAULT_V03_MAPPING.md` — 용어 ↔ 코드 위치 대응표
- `HARBOR_WEBHOOK_EVENTS.md` — Harbor webhook 기반 scan 트리거 (참고)
