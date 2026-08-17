# NodeVault Constitution

<!--
  ②-form (D-12), REPOSITORY-MIRROR variant. NodeVault does NOT independently
  own platform-wide canonical meaning. The platform authority is selected by the
  external Authority Router; this repository carries a revision-pinned mirror so
  repo-local agents can work fail-closed without inventing a second authority.

  Authority revision: AR-2026-08-17.1
  Repository mirror: docs/PLATFORM_MASTER_DESIGN.md §4.1–§4.10 only
  Verification record: docs/AUTHORITY_MIRROR_VERIFICATION.md
-->

## Repository Authority Mirror — AR-2026-08-17.1

Cross-repo platform meaning is selected outside this repository by the current
Authority Router and its scoped platform authority chain. For this revision:

- platform invariants: `Platform Spec Wiki — CURRENT / 1. constitution`
- platform structure / responsibility / call direction:
  `Platform Spec Wiki — CURRENT / 2. architecture`
- repository-portable invariant mirror: `docs/PLATFORM_MASTER_DESIGN.md §4.1–§4.10`
- mirror verification record: `docs/AUTHORITY_MIRROR_VERIFICATION.md`

`docs/PLATFORM_MASTER_DESIGN.md` is a **repository mirror**, not an independent
platform canonical. Its VERIFIED authority scope is limited to the exact scope
and blob recorded in `docs/AUTHORITY_MIRROR_VERIFICATION.md`; content elsewhere
in the same file is context/evidence unless separately routed by the task
`Authority Snapshot`.

A task may consume the repository mirror for cross-repo invariant meaning only
when **all** of the following are true:

1. the task `Authority Snapshot` declares `AR-2026-08-17.1`;
2. `docs/AUTHORITY_MIRROR_VERIFICATION.md` says `SYNC STATUS: VERIFIED`;
3. the mirror blob SHA matches the blob SHA recorded by that verification record;
4. every additional scoped/domain/component authority required by the task is
   explicitly present in the task `Authority Snapshot`;
5. no semantic conflict with the current Authority Router/upstream authority has
   been detected.

If any condition is missing, stale, unknown, mismatched, or conflicting, stop
with `AUTHORITY_CONFLICT`. Do not choose whichever document is newer or more
conveniently available. **Revision equality alone is not sufficient.**

The current repository verification record does **not** mirror platform
architecture/ownership/call-direction. Work that needs those semantics must
carry `Platform Spec Wiki — CURRENT / 2. architecture` (and any required
component/capability contract) directly in the task `Authority Snapshot`.

This spec-kit constitution does not restate or fork the platform-wide meaning.
NodeVault code that touches those concerns is bound by the task's current
Authority Snapshot, using the verified repository mirror only when the gate
above passes, and by the repo's detailed operating rules in `CLAUDE.md` /
`AGENTS.md`, which this file does not duplicate.

## Process discipline (repo-operational — owned by this repo)

- **Deterministic gates are the guarantee.** Merge is decided by deterministic
  checks; NodeVault has an active branch ruleset with 11 required checks (the
  richest on the platform). LLM/agent review is advisory: a passing review never
  merges alone, a failing required gate is never overridden.
- **Operating boundaries are in `CLAUDE.md` / `AGENTS.md`** (responsibility
  boundary, package-direction rules, the decision checklists, zero-warning lint
  baseline). This constitution references them; it does not restate them.
- **Spec-anchored change**; **test-first** (see `CLAUDE.md` §9 validation matrix).
- **Small diffs, no unrelated refactors** (`CLAUDE.md` §7).
- **Branch protection**: `main` lands via PR with the required checks below; no
  direct pushes.

## Repo-local enforced constraints (derived index — NOT canonical)

> Derived index of THIS repo's own gates. Not canonical — SoT is the gate
> itself (Makefile / CI). All are IMPLEMENTED: NodeVault's ruleset makes them
> required checks, so a failure blocks merge.

- **golangci-lint** (IMPLEMENTED — `make lint`, required check "Lint";
  zero-warning baseline per `CLAUDE.md` §8).
- **Unit Tests** (IMPLEMENTED — `make test`, required check "Unit Tests").
- **govulncheck** (IMPLEMENTED — `make vuln`, required check "Vulnerability Scan (govulncheck)").
- **kube-linter** (IMPLEMENTED — `make kube-linter`, required check "kube-linter").
- **Build** (IMPLEMENTED — required check "Build").
- **Module Drift** (IMPLEMENTED — required check "Module Drift").
- **Proto Lint (Buf)** (IMPLEMENTED — required check "Proto Lint (Buf)").
- **kube-slint Gate** (IMPLEMENTED — required check "kube-slint Gate": operational-SLI guardrail).
- **Actionlint** (IMPLEMENTED — required check "Actionlint").
- **CodeQL** (IMPLEMENTED — required checks "Analyze go" / "Analyze actions").

## §1.10 — "do not record what you did not observe"

**Authority: CURRENT platform invariant under `AR-2026-08-17.1`.** NodeVault's
local enforcement is a separate axis. This repo may record IMPLEMENTED only for
checks that are actually backed by required status checks; otherwise record the
observed state (for example `PROPOSED (runs, not merge-enforced)`) rather than
inventing enforcement.

## Governance

Cross-repo semantics cannot be amended by editing this constitution, the
repository mirror, or the verification record alone. They follow the task's
current Authority Snapshot. A new platform authority revision must be accepted
before the repository mirror is synchronized and independently re-verified.

**Version**: 2.1.0 | **Ratified**: 2026-08-02 | **Last Amended**: 2026-08-17