# Coverage regression policy

Resolves the design questions in issue #50. This is NodeVault-specific — the
common Go Baseline intentionally does not mandate the same numeric threshold
or scope rule for every repository (see the cross-repo guardrail Notion doc).

## 1. Merge-blocking coverage scope

Packages under `pkg/` only. This is where NodeVault's actual business logic
(build orchestration, index, catalog, validation, reconcile, ...) lives.

## 2. Generated code

Excluded, by a reproducible structural rule: any file whose path has `protos`
as a path segment. NodeVault keeps all protoc-generated stubs under the
top-level `protos/` directory (see `CLAUDE.md`'s package structure) and never
hand-writes code there, so this is exact — no header-comment sniffing needed.
Measured effect: including `protos/` drags the raw `./...` aggregate from
~69% down to ~39% (2026-07-24 measurement), which would make a single
repo-wide number meaningless.

`cmd/` (binary entrypoints — flag parsing, wiring, `main()`) is excluded for
the same reason it's excluded from the common Baseline's per-package
expectations elsewhere: it's thin glue verified more by the integration/smoke
gates than by unit coverage, and its low coverage (`cmd/controlplane` was
16.6% at measurement time) isn't a meaningful regression signal on its own.
`cmd/` coverage is still produced and uploaded in the existing coverage
artifact — just not part of this gate's scope.

## 3. Comparison baseline

A checked-in baseline file (`.coverage-baseline.yaml`), not a downloaded
main-branch artifact or a fixed floor. Reasoning:

- A downloaded-artifact comparison adds a network/API dependency and a
  race (which run counts as "current main") for a check that doesn't need
  either.
- A fixed floor never adapts as real coverage improves, so it either lags
  (too loose) or needs constant manual bumping with no record of why.
- A checked-in file is reviewable in the PR diff — a baseline bump is a
  visible, deliberate line change with a commit message, not a silent
  runtime comparison.

## 4. Tolerance

1.0 percentage point below baseline. Chosen because `pkg/`'s current
statement count is large enough (thousands of statements across ~19
packages) that normal single-PR fluctuation from adding a few covered or
uncovered lines is well under 1 point; a real regression — an entire new
code path added without tests — moves the aggregate by more than that.

## 5. Reviewing intentional reductions

Any PR that lowers `.coverage-baseline.yaml`'s `baseline_percent` is a
visible diff hunk in review, same as any other checked-in policy value. No
separate approval workflow is added — normal PR review is the review.

## Rollout status

Per issue #50's acceptance criteria, this check (`cmd/coveragegate`, wired as
the `Coverage Regression` CI job) is deliberately **not** added to the branch
ruleset's required checks yet. It runs and reports on every PR so its
pass/fail behavior can be observed on real changes first. Promote it to a
required check only after that observation window, as its own follow-up
(update the ruleset via the GitHub UI or `gh api`, same as the other required
checks — see issue #35's audit trail for how the existing ones were added).
