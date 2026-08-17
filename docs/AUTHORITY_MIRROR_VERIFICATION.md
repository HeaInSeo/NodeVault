# Repository Authority Mirror Verification Record

DOCUMENT CLASS: MIRROR VERIFICATION RECORD  
AUTHORITY REVISION: `AR-2026-08-17.1`  
SYNC STATUS: **VERIFIED**  
VERIFIED AT: `2026-08-17T18:15+09:00`  
REVIEWER: internal design-authority re-verification after external findings EXT-01 / EXT-02

## Verified mirror

- repository: `HeaInSeo/NodeVault`
- mirror path: `docs/PLATFORM_MASTER_DESIGN.md`
- verified mirror scope: **§4.1–§4.10 only**
- mirror blob SHA: `2dc65b2c255a04c5db0b1b98b07f700622afa23a`

The rest of `PLATFORM_MASTER_DESIGN.md` is useful repository context/evidence but is **not covered by this VERIFIED mirror record** and must not be treated as platform authority merely because it is in the same file.

**Consumption precedence:** the inline wording inside `PLATFORM_MASTER_DESIGN.md` is descriptive only. Whether that file may be consumed as authority is decided by the current Authority Router, the task `Authority Snapshot`, this verification record, and the exact recorded blob/scope. Therefore a matching revision string inside the mirror file is never sufficient by itself.

## Upstream authority snapshot

Meta-governance / routing:
- `00. Current Authority Router — 문서 권위·진입점`
- Notion page id: `3bf86e6d-d1d1-8110-938b-c358e17c779e`

Platform invariant authority:
- `Platform Spec Wiki — CURRENT / 1. constitution — 불변 결정`
- Notion page id: `3ae86e6d-d1d1-811c-8bfa-d78e5f878f60`
- verified scope: §1.1–§1.10

Platform structure / responsibility / call-direction authority:
- `Platform Spec Wiki — CURRENT / 2. architecture — 시스템 구조`
- Notion page id: `3ae86e6d-d1d1-81e7-a1bc-ee14daa13f22`
- **NOT MIRRORED by this record.** Tasks that need architecture/ownership/call-direction semantics must carry that exact scoped authority in their task `Authority Snapshot`; they may not infer it from `PLATFORM_MASTER_DESIGN.md`.

## Semantic comparison result

The invariant clauses mirrored in `PLATFORM_MASTER_DESIGN.md` §4.1–§4.10 were compared against the CURRENT constitution §1.1–§1.10 for authority ownership and normative meaning.

- §4.1 reproducibility ↔ §1.1 reproducibility: aligned for no-`latest`, pinning, and `casHash` execution pin.
- §4.2 `casHash` ↔ §1.2: aligned.
- §4.3 `stableRef` ↔ §1.3: aligned for the `stableRef`/`casHash` usage boundary and 1:N cardinality. The `DagEdit RunnerNode` sentence has no corresponding normative clause in §1.3 and is excluded from VERIFIED platform-invariant meaning.
- §4.4 artifact dual-axis ↔ §1.4: aligned for lifecycle vs integrity separation, values/ownership, and Active-based catalog exposure. The lifecycle transition examples/graph and forbidden-transition rules have no corresponding normative clause in §1.4 and are excluded from VERIFIED platform-invariant meaning.
- §4.5 write authority ↔ §1.5: aligned.
- §4.6 OCI referrer split ↔ §1.6: aligned for separation of declared/profile/security metadata; implementation-status annotations are evidence, not mirrored authority.
- §4.7 sori boundary ↔ §1.7: aligned for the protocol/meaning boundary and call direction. Upstream §1.7 content not reproduced in the mirror, including volume-index ownership, remains upstream authority and is not negated by omission.
- §4.8 image build ↔ §1.8: aligned for in-process podbridge5, rootless operation, and no privileged fallback; verification dates/tests are evidence, not mirrored authority.
- §4.9 ResolveRecipe ↔ §1.9: aligned on responsibility boundary: NodeVault resolves/returns candidates, NodeKit/user selects/confirms where choice exists, and submit enforces a complete pin. Examples and cache-hit mechanics are evidence/context, not a transfer of selection authority.
- §4.10 ↔ §1.10: §4.10 is intentionally a pointer; the CURRENT constitution owns the invariant.

## Known exclusions from VERIFIED authority

The following content is **not** promoted to authority by this record even when present inside the mirror file:

- implementation-complete / proposed / sprint labels;
- dates, test names, issue numbers, deployment addresses, cluster facts;
- operational examples and evidence;
- component inventory or roadmap statements outside §4.1–§4.10;
- any architecture/ownership/call-direction statement whose authority belongs to CURRENT `2. architecture`;
- any capability/component contract not explicitly present in the task `Authority Snapshot`;
- any statement inside §4.1–§4.10 that lacks a corresponding normative upstream §1.x clause — including §4.3's `DagEdit RunnerNode` sentence and §4.4's lifecycle transition graph/forbidden transitions — is historical/component evidence, not a VERIFIED platform invariant;
- any normative upstream clause not reproduced in the mirror — including §1.7's volume-index ownership rule — remains owned by the upstream CURRENT authority and must not be inferred away from its omission here.

## Consumption gate

A repository agent may consume the verified mirror for cross-repo invariant meaning **only if all conditions are true**:

1. task `Authority Snapshot.authority_revision == AR-2026-08-17.1`;
2. this record says `SYNC STATUS: VERIFIED`;
3. `docs/PLATFORM_MASTER_DESIGN.md` has blob SHA `2dc65b2c255a04c5db0b1b98b07f700622afa23a`;
4. the task includes every additional scoped/domain/component authority required by the work;
5. no semantic conflict with the current Authority Router/upstream authority has been detected.

Otherwise: `AUTHORITY_CONFLICT` and stop cross-repo semantic work. Do not resolve by timestamp, filename, search rank, or local convenience.

## Invalidation

This record becomes **STALE** immediately when any of the following occurs:

- the CURRENT constitution receives a normative edit within the verified scope;
- the verified mirror blob changes;
- the Authority Revision changes;
- a task needs a scope not covered by this record;
- a semantic conflict is discovered.

A new comparison and a new verification record/revision are required before `SYNC STATUS` can return to `VERIFIED`.