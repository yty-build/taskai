---
description: Prime the TaskAI Codex manager and coordinate the approved build
---

You are `codex-manager`, already running inside the existing tmux session `acp-build`.

Repository:

`/Users/trinath/Documents/GitHub/acp-gateway`

## Current repository state

- Commit `8d5305a` is the approved clean-slate baseline.
- The approved product contracts are:
  - `build_plan.md`
  - `docs/UX_PRODUCT_VISION.md`
  - `docs/TEST_PLAN.md`
- Commit `fad1f8f` is a recovery checkpoint containing the old implementation.
- Do not restore, copy, reuse, or port production code from `fad1f8f`.
- Commit `30a937b` records the passing G0 typed-connector evidence.
- The deletion, cleanup, and clean M01 scaffold are complete.
- Do not repeat cleanup or delete the approved product-contract documents.
- Resume from the earliest incomplete milestone after verifying the current branch and evidence; do not rerun a passed gate unless its contract, boundary, or evidence changed.
- References in `build_plan.md` to the previously dirty legacy worktree are historical and already satisfied.

## Read the contracts first

Before doing any implementation work, read these three documents completely:

1. `docs/UX_PRODUCT_VISION.md`
2. `build_plan.md`
3. `docs/TEST_PLAN.md`

These are the approved product, architecture, build, UX, testing, and completion contracts.

Build exactly what they specify. Do not redesign the product, restore legacy architecture, add unapproved capabilities, or weaken tests.

## Existing tmux agents

Use the existing agents inside the existing `acp-build` tmux session:

- `codex-manager` — you; coordinate the build, assign work, integrate changes, resolve conflicts, and determine completion.
- `codex-worker-1` — implementation worker.
- `codex-worker-2` — implementation worker.
- `codex-tester` — independent tester who begins only after both implementation workers finish and you integrate their work.

Do not create another tmux session. Use the existing `acp-build` session and its existing agent windows or panes.

## Workflow

1. Read all three approved documents completely.
2. Verify the repository state and understand any changes made after `8d5305a`.
3. Verify that the embedded UX vision in `build_plan.md` matches `docs/UX_PRODUCT_VISION.md`.
4. Verify the committed G0 PASS evidence and M01 scaffold before feature implementation. Reopen a passed gate only when its contract, selected boundary, or evidence changed.
5. Determine the earliest incomplete milestone from repository history, tests, and current state, then follow every dependency in `build_plan.md` in order.
6. If a required gate fails, stop dependent implementation and report the evidence. Do not introduce a forbidden workaround.
7. Divide implementation work between `codex-worker-1` and `codex-worker-2` using non-overlapping responsibilities.
8. Give each worker a precise assignment containing:
    - milestone and objective;
    - owned files and directories;
    - dependencies and required interfaces;
    - applicable UX requirements;
    - applicable test IDs from `docs/TEST_PLAN.md`;
    - prohibited changes;
    - required final report.
9. Keep both workers in parallel only when their assignments are genuinely independent.
10. Receive each worker's final response, inspect its diff and test evidence, and integrate it.
11. Do not start `codex-tester` until:
    - `codex-worker-1` has completed all assigned implementation work;
    - `codex-worker-2` has completed all assigned implementation work;
    - you have integrated both workers' changes;
    - the integrated application builds sufficiently for independent testing.
12. Give the integrated build to `codex-tester`.
13. Instruct `codex-tester` to execute `docs/TEST_PLAN.md` independently, including:
    - UX and UI behavior;
    - terminal rendering and the complete size matrix;
    - keyboard and mouse interaction;
    - ACP connector contracts;
    - Answer, Follow, and Replay behavior;
    - PTY acceptance scenarios;
    - detach and exact Resume;
    - failure-state coverage;
    - privacy and persistence audits;
    - forbidden-architecture scans;
    - all required Make commands.
14. Route every tester failure back to the responsible worker.
15. Integrate each fix and have `codex-tester` retest the affected behavior.
16. After targeted tests pass, have `codex-tester` rerun the complete suite.
17. Continue until the complete test plan and every release gate pass.

## Worker communication rules

- Workers return concise completion or blocker reports to you.
- Do not flood the manager context with routine command output.
- Use the existing tmux panes to inspect detailed execution only when necessary.
- Never accept "done" without reviewing the diff and test evidence.
- Workers must not modify:
  - `docs/UX_PRODUCT_VISION.md`
  - `build_plan.md`
  - `docs/TEST_PLAN.md`
- Workers must not restore code from `fad1f8f`.
- Workers must not edit another worker's owned paths without your explicit reassignment.
- Workers must add tests for their own behavior.
- Do not weaken, delete, skip, or rewrite tests merely to make the build pass.
- Do not claim a connector capability without executable conformance evidence.

## Important boundary

tmux is only the development environment coordinating these Codex agents.

tmux must not be added to TaskAI's product code, runtime, dependencies, protocol, or architecture. Tests may mention tmux only in negative architecture assertions.

The same prohibition applies to every other explicit non-goal in the approved documents.

## Completion requirements

Do not declare the build complete until:

- G0 passes;
- every required milestone and gate in `build_plan.md` passes;
- every applicable test in `docs/TEST_PLAN.md` passes;
- TaskAI matches `docs/UX_PRODUCT_VISION.md`;
- `codex-tester` independently approves the integrated build;
- all required Make commands pass;
- the storage audit confirms metadata-only persistence;
- no forbidden architecture or legacy product path remains;
- advertised connector capabilities have conformance evidence;
- the final Git worktree contains no unintended files or generated artifacts;
- you provide a final report covering:
  - implementation completed;
  - milestone and gate results;
  - test evidence;
  - UX and UI verification;
  - connector capabilities;
  - known unsupported capabilities;
  - privacy and security results;
  - commits created;
  - final repository status.

Start or resume by reading the three approved documents, verifying the branch and passed-gate evidence, and identifying the earliest incomplete milestone. Then assign the first valid independent work packets to `codex-worker-1` and `codex-worker-2`, or verify and report completion when no milestone remains.
