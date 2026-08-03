# NodeVault Constitution

<!--
  ②-form (D-12), CANONICAL-HOST variant. NodeVault is special: it HOSTS the
  platform canonical constitution. The cross-repo invariants are DEFINED here,
  in docs/PLATFORM_MASTER_DESIGN.md §4 — not referenced from elsewhere. This
  spec-kit constitution therefore does NOT restate §4; it points to it as the
  source it owns, and adds only NodeVault's repo-local process discipline and
  gate index. SoT for the local gates is the gates themselves (Makefile / CI),
  and SoT for the cross-repo invariants is §4 in this repo.
-->

## This repo HOSTS the platform canonical (docs/PLATFORM_MASTER_DESIGN.md §4)

The platform's cross-repo invariants live in **this repository** at
**`docs/PLATFORM_MASTER_DESIGN.md` §4** ("이 섹션의 결정은 명시적 아키텍처 논의
없이 변경할 수 없다"): §4.1 reproducibility · §4.2 casHash · §4.3 stableRef ·
§4.4 artifact dual-axis (`lifecycle_phase` / `integrity_health`) · §4.5 write
authority · §4.6 OCI referrer split · §4.7 sori boundary · §4.8 image build ·
§4.9 ResolveRecipe. Every other repo's constitution *references* §4; NodeVault
is the **source of truth**.

This spec-kit constitution does not restate or fork §4 — §4 is authoritative.
NodeVault code that touches those concerns is bound by §4 directly (and by the
repo's detailed operating rules in `CLAUDE.md` / `AGENTS.md`, which this file
does not duplicate). On any conflict, §4 wins.

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

**Status: PROPOSED.** §1.10 is a platform-level rule that is **not yet part of
§4** (a `docs/PLATFORM_MASTER_DESIGN.md` **§4.10** addition is pending — and
because NodeVault hosts §4, adding it is a NodeVault docs task). NodeVault has
no deterministic rule enforcing it today; marked PROPOSED, not IMPLEMENTED,
until such a rule exists.

## Governance

Versioned and amendable. Amendment procedure: (1) rationale; (2) when a local
rule's status changes, update the enforcing gate in the same change; (3) bump
the version below — major = a principle/rule removed or redefined or the source
of authority changed, minor = rule added, patch = clarification. A rule is
IMPLEMENTED only if a deterministic (here: required-check) gate enforces it;
otherwise PROPOSED. **Cross-repo invariants are governed by §4 in this repo, via
its own "명시적 아키텍처 논의" amendment rule — not by this spec-kit file.**

**Version**: 1.0.0 | **Ratified**: 2026-08-03 | **Last Amended**: 2026-08-03
