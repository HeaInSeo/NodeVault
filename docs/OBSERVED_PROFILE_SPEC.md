# Observed Profile Spec — `toolprofile` referrer

버전: v0.1.0-draft
작성일: 2026-05-03
상태: Sprint 0 (코드 변경 없음 — 스펙 선언)
근거: `NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md` §3.1, §4.3, §5.4

---

## 1. artifactType

```
application/vnd.nodevault.toolprofile.v1+json
```

`toolspec` referrer (`application/vnd.nodevault.toolspec.v1+json`)와 별도로 분리된다.

**분리 이유**: `toolspec`은 등록 시점 declared metadata (변경 안 됨). `toolprofile`은 dry-run 실행 결과 observed metadata (fixture/profile 정책 변경 시 갱신). 한 artifact에 묶으면 한 축만 갱신되어도 다른 축까지 교체해야 한다.

---

## 2. Payload 구조

### 2.1 성공 케이스 (`validationStatus: "succeeded"`)

```json
{
  "artifactType": "application/vnd.nodevault.toolprofile.v1+json",
  "subject": {
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": "sha256:IMAGE_DIGEST"
  },
  "profile": {
    "casHash": "sha256:TOOLDEFINITION_CAS_HASH",
    "validationHash": "sha256:VALIDATION_HASH",
    "validationStatus": "succeeded",
    "validationRun": {
      "mode": "dry-run",
      "runnerScriptDigest": "sha256:SCRIPT_DIGEST",
      "sampleDataRefs": [
        { "port": "reads", "uri": "s3://...", "digest": "sha256:..." }
      ],
      "command": "bwa mem ref.fa reads.fastq",
      "exitCode": 0
    },
    "observedIoProfile": {
      "inputs": [
        { "port": "reads", "fileCount": 2, "nonEmpty": true }
      ],
      "outputs": [
        { "port": "alignment", "fileCount": 1, "nonEmpty": true, "comparatorResult": "pass" }
      ]
    },
    "observedResourceProfile": {
      "peakCpuMillicores": 1200,
      "peakMemoryMiB": 512,
      "durationSeconds": 42,
      "diskReadMiB": 180,
      "diskWriteMiB": 95
    },
    "contractCheck": {
      "allOutputsPresent": true,
      "comparatorResult": "pass",
      "details": []
    },
    "resourceRecommendation": {
      "requestCpu": "500m",
      "requestMemory": "256Mi",
      "limitCpu": "2000m",
      "limitMemory": "1Gi"
    },
    "profileStatus": "observed"
  }
}
```

### 2.2 인프라 장애 케이스 (`validationStatus: "infra_failed"`)

```json
{
  "artifactType": "application/vnd.nodevault.toolprofile.v1+json",
  "subject": {
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": "sha256:IMAGE_DIGEST"
  },
  "profile": {
    "casHash": "sha256:TOOLDEFINITION_CAS_HASH",
    "validationHash": null,
    "validationStatus": "infra_failed",
    "failureReason": "timeout",
    "observedResourceProfile": {
      "timeout": true,
      "timeoutSeconds": 1800
    },
    "profileStatus": "inconclusive"
  }
}
```

---

## 3. `validationHash` 운영 규칙

### 3.1 생성 조건

`validationHash`는 **successful functional validation에 대해서만 생성**한다.

### 3.2 hash 입력에 포함하는 것

| 항목 | 이유 |
|------|------|
| `validationRun.mode` | 실행 방식 (dry-run / smoke / profiler) |
| `subject.digest` (imageDigest) | 어떤 image를 검증했는지 |
| `validationRun.runnerScriptDigest` | 실행 스크립트 동일성 |
| `validationRun.sampleDataRefs` digest 목록 | 입력 데이터 동일성 |
| `validationRun.command` | 실행 명령 |
| `validationRun.exitCode` | 성공 여부 |
| `observedIoProfile` 결정론적 요약 | fileExists, fileCount, nonEmpty, comparatorResult |
| `contractCheck.allOutputsPresent` | 출력 계약 이행 여부 |
| `contractCheck.comparatorResult` | 비교 결과 요약 |

### 3.3 hash 입력에서 제외하는 것 (환경 종속 측정값)

| 항목 | 제외 이유 |
|------|----------|
| `peakCpuMillicores`, `avgCpu` | 노드·부하 의존, 재현 불가 |
| `peakMemoryMiB` | 환경 의존 |
| `durationSeconds` | 실행 시간 |
| `diskReadMiB`, `diskWriteMiB` | 환경 의존 |
| node name, cpu model | 인프라 식별 정보 |
| total memory | 환경 의존 |
| raw stdout/stderr | 대용량, 비결정론적 가능 |
| timestamp | 시간 의존 |

**원칙**: 같은 tool + 같은 data + 같은 script → 같은 validationHash. 환경 차이(노드 성능, 클러스터 부하)는 hash에 영향을 주지 않는다.

### 3.4 infra-level failure 정의

다음 실패는 tool의 functional behavior 검증이 아니므로 `validationHash` 미생성:

- OOMKilled
- timeout (activeDeadlineSeconds 초과)
- node eviction
- pod scheduling failure
- image pull failure
- registry pull error
- SIGTERM/SIGKILL 기반 종료
- cluster/network/storage transient failure

이 경우 `validationStatus: "infra_failed"`, `profileStatus: "inconclusive"`.

### 3.5 application-level failure 정책

tool 자체가 정상적으로 exit code 1/2 등으로 실패를 보고한 경우:

- **기본 정책**: `validationHash` 미생성
- **확장 정책 (옵션)**: `expected-failure fixture`가 명시적으로 정의된 경우에만 허용

---

## 4. Timeout 정책

| 항목 | 값 |
|------|-----|
| 기본 timeout | 30분 |
| timeout 발생 시 `validationHash` | 미생성 |
| timeout 발생 시 `validationStatus` | `"infra_failed"` |
| timeout 발생 시 `profileStatus` | `"inconclusive"` |
| timeout 기록 위치 | `observedResourceProfile.timeout: true`, `observedResourceProfile.timeoutSeconds` |

L4 smoke run (`pkg/validate/service.go:smokeTimeout = 5*time.Minute`)은 기동 확인 전용이다. Validator/Profiler(Sprint 2)는 별도 timeout 정책을 가진다.

---

## 5. Referrer Retention / GC

### 5.1 index.Entry 캐시

`pkg/index/schema.go:Entry`에 `ObservedProfileDigest` 필드를 추가한다 (Sprint 1).

```go
ObservedProfileDigest string `json:"observed_profile_digest,omitempty"`
```

- latest toolprofile referrer digest 1개만 캐시
- 갱신 주체: Validator/Profiler (Sprint 2~) 또는 NodeSentinel

### 5.2 Registry 보존

| 정책 | 값 |
|------|-----|
| 유지 개수 | latest 3개 |
| 초과분 처리 | GC candidate 표시 (즉시 삭제 안 함) |
| 삭제 | registry GC 정책에 위임 |

임상·운영 evidence가 붙은 profile artifact는 자동 삭제하지 않고 manual review 대상으로 둔다.

---

## 6. 구현 경로

| 단계 | 내용 | Sprint |
|------|------|--------|
| `pkg/oras/referrer.go` | `PushToolProfileReferrer` 추가 | Sprint 1 |
| `pkg/index/schema.go` | `ObservedProfileDigest` 필드 추가 | Sprint 1 |
| `pkg/index/store.go` | `SetObservedProfileDigest` 추가 | Sprint 1 |
| Validator/Profiler 패키지 | sample data 실행 + profile 수집 | Sprint 2 |
| `pkg/build/service.go` | Profiler hook 연결 | Sprint 2 |

---

## 7. 관련 문서

- `TOOL_CONTRACT_V0_3_DRAFT.md` — 상위 계약 (v0.3 additive field 명세)
- `SECURITY_SCAN_SPEC.md` — `security` referrer (독립 분리)
- `NODEVAULT_V03_MAPPING.md` — 용어 ↔ 코드 위치 대응표
- `RUNNER_NODE_SPEC.md` — DagEdit RunnerNode에서 `validationHash`/`observedProfileDigest` 사용
