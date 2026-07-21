# NodeVault Authority Map (TODO-09a)

작성일: 2026-04-15  
상태: **확정**  
선행 조건: TODO-01, TODO-02

---

## 1. 설계 원칙

**NodeVault build 완료는 trigger일 뿐.**  
Index commit 판단과 실행은 NodeVault only.  
이 경계가 깨지면 lifecycle_phase 변경 authority가 분산된다.

---

## 2. Write Authority Map

| 작업 | 소유자 | 비고 |
|------|--------|------|
| Build Job 실행 | NodeVault (위임) | kaniko/buildah Job 오케스트레이션 |
| Build 완료 이벤트 | NodeVault → NodeVault | BuildEvent stream (gRPC) |
| Index commit 판단 | **NodeVault only** | L4 통과 여부 판단 포함 |
| Index append | **NodeVault only** | `pkg/index.Store.Append()` 호출 |
| `lifecycle_phase` 변경 | **NodeVault only** | `SetLifecyclePhase()` — 운영 의도 축 |
| `integrity_health` 변경 | **reconcile loop only** | `SetIntegrityHealth()` — 관찰 결과 축 |
| Retract (Active→Retracted) | **NodeVault only** | 운영자 명시적 호출 |
| Delete (Retracted→Deleted) | **NodeVault only** | Harbor GC 완료 후 |
| Catalog 노출 결정 | `lifecycle_phase = Active` 기준만 | `integrity_health`는 노출에 영향 없음 |
| Index read (조회) | Catalog 서비스 (read-only) | `pkg/index.Store` 경유 |
| Index write (직접 파일 접근) | **금지** | 반드시 `pkg/index` 패키지 경유 |

---

## 3. NodeVault → NodeVault 핸드오프 프로토콜

```
NodeVault                          NodeVault
    │                                  │
    │── SubmitToolBuild() gRPC ───────▶│  (legacy BuildAndRegister() removed, issue #15)
    │                                  │
    │  [빌드 Job 실행]                  │
    │  [L2 push 완료]                   │
    │  [L3 dry-run]                    │
    │  [L4 smoke run]                  │
    │                                  │
    │◀── BUILD_EVENT_KIND_DIGEST_ACQUIRED │
    │◀── BUILD_EVENT_KIND_SUCCEEDED ──  │
    │                                  │
    │  [RegisterTool 호출]             │
    │── RegisterTool() gRPC ─────────▶│
    │                                  │
    │                         [index.Append()]
    │                         [lifecycle_phase = Active]
    │                         [integrity_health = Healthy]
    │                                  │
    │◀── RegisterToolResponse ────────  │
```

**핵심**: Build 완료 이벤트(SUCCEEDED) 수신 후 NodeVault가 RegisterTool을 독립적으로 호출한다.  
NodeVault는 build 파이프라인만 실행하고 index 상태를 직접 변경하지 않는다.

---

## 4. Reconcile Loop Authority

```
reconcile loop
    │
    ├── Harbor API 조회 (Exists / Attached / Complete / Reachable)
    │
    ├── SetIntegrityHealth(casHash, Healthy | Partial | Missing | Unreachable | Orphaned)
    │
    └── SetLifecyclePhase() 호출 금지 ← 절대 불변 규칙
```

reconcile loop는 관찰 결과만 기록한다. 운영 의도(`lifecycle_phase`)는 절대 건드리지 않는다.

---

## 5. Catalog 서비스 Read Authority

Catalog 서비스는 **read-only**다.

| 허용 | 금지 |
|------|------|
| `store.ListActive()` | `store.Append()` |
| `store.ListByStableRef()` | `store.SetLifecyclePhase()` |
| `store.GetByCasHash()` | `store.SetIntegrityHealth()` |
| `catalog.Load(casHash)` | 직접 파일 접근 |

---

## 6. lifecycle_phase 전이 — NodeVault 명시적 호출만

```
Pending  ──[L4 통과 + RegisterTool]──▶ Active
Active   ──[운영자 Retract 요청]──────▶ Retracted
Retracted ─[운영자 Restore 요청]──────▶ Active
Retracted ─[운영자 Delete + Harbor GC]▶ Deleted
```

금지 전이 (운영 사고 방지):
- `Pending → Retracted` ✗
- `Active → Deleted` ✗ (반드시 Retracted 거침)

---

## 7. 완료 기준 체크리스트 (TODO-09a)

- [x] write authority 범위 문서화
- [x] NodeVault 하위 책임 경계 명시
- [x] lifecycle_phase 변경 authority 귀속 명시 (NodeVault only)
- [x] integrity_health 변경 authority 귀속 명시 (reconcile loop)
- [x] Delete / Retract authority 귀속 명시
- [x] index mutation authority 귀속 명시
- [x] NodeVault build 완료 → NodeVault index commit 핸드오프 프로토콜 명시
- [x] 결과물: authority map 표 (구두 합의 아님)
