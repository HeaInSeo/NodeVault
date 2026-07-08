# NodeVault — Agent Guidelines

This file applies to Codex and other coding agents working in this repository.

## Cost Boundary

Do not start paid or usage-metered workflows automatically. In particular, do
not spawn multi-agent review/research workflows unless the user explicitly asks
for that run in the current session.

For major tag or release work, use only the repository's normal local/CI
checks by default. If a multi-agent review would be useful, report that
recommendation and wait for explicit user approval before starting it.

## Major Tag Updates

A major tag update means creating, moving, or preparing a release tag such as
`v1.0.0`, `v2.0.0`, or another project release tag that changes the supported
platform contract.

Before completing major tag work:

- Confirm the working tree is clean or identify unrelated local changes.
- Do not use NodePort for NodeVault live validation; use the existing ClusterIP
  Service plus `kubectl port-forward` when live gRPC access is needed.
- Run the normal non-LLM validation gates that apply to the touched scope, such
  as `make lint`, `make test`, `make kube-lint`, and the existing GitHub
  Actions CI.
- Report any skipped validation explicitly with the reason.

## Multi-Agent Review

The recommended high-rigor review shape is Find -> Verify -> Synthesize:

- Find: inspect independent risk areas such as consistency, error handling,
  security boundaries, test accuracy, docs-code drift, and API naming.
- Verify: challenge each finding against direct `file:line` evidence and drop
  uncertain findings.
- Synthesize: separate confirmed issues into fix-now items, guardrail-rule
  candidates, and deferrable work.

This workflow is not automatic. It must not run unless the user explicitly
approves the extra agent usage.
