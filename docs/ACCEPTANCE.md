# V1 acceptance evidence map

This document maps the specification's acceptance checklist to deterministic automation and the separate authenticated/manual matrix. It does not convert external provider availability into a fake green result.

## One-command deterministic acceptance

```bash
make acceptance
```

The command:

1. runs every Go test uncached;
2. reruns every test under the race detector;
3. checks formatting and `go vet`;
4. builds the real `cmd/acpctl` binary with fixed version/commit/date values and verifies exact `acpctl version` output;
5. runs tracked-file, credential-pattern, ignore-rule, fixture-mode, and two-table entity-boundary checks.

The black-box lifecycle test builds `acpctl`, uses an isolated temporary `AGENT_HOME` and Git repository, launches a genuinely detached hidden worker against deterministic fake `acpx`, prompts/follows/stops/resumes it, uses fake provider-native resume, creates an independent review, and validates persisted/logged behavior. No network or provider credential is required.

Primary end-to-end evidence:

- `internal/cli/lifecycle_integration_test.go`
- `internal/worker/worker_test.go`
- `internal/api/server_test.go`
- `internal/web/server_test.go`
- `internal/console/model_test.go`
- `internal/service/session_test.go`
- `internal/service/review_test.go`
- `internal/service/stream_test.go`

## Core checklist

| Specification criterion | Deterministic evidence | Authenticated/manual evidence |
| --- | --- | --- |
| Create, list, and view a Task | `TestTaskCommandLifecycle`, `TestVersionedTaskAndSessionAPI` | CLI/web smoke |
| Start multiple Sessions under one Task | `TestStartProtectsSharedCheckoutAndExplicitOverride`, `TestStartCreatesDistinctPersistentWorktrees`, API test | Provider matrix |
| Associate Session with agent and repo | `TestStartPersistsPermissionAndSendsCompleteInitialPrompt`, lifecycle test | Provider matrix |
| Start Codex/Claude/Gemini/Grok/OpenCode through acpx | `TestLoadEmbeddedProfiles` verifies exact profiles; adapter/worker fixtures verify common ACP contract | Required T08 live matrix for each authenticated CLI |
| Start custom ACP-compatible command | `TestUserProfilesOverlayDefaultsAndAddCustomCommands`, `TestCustomCommandIsPassedAsOneLiteralAgentArgument` | Optional local custom-agent smoke |
| Session survives parent CLI exit | `TestDetachedHiddenWorkerBecomesReachableAndStopsCleanly`, lifecycle test | Provider matrix detach |
| Prompt existing Session | lifecycle test, API test | Provider follow-up |
| Initial prompt contains Task, role, repo/worktree | `TestStartPersistsPermissionAndSendsCompleteInitialPrompt`, lifecycle argv log | Provider matrix |
| Explicit unattended write permission | `TestPrepareDefaultsToReadApprovalAndValidatesProfileAndDirectory`, `TestPermissionMetadataDefaultsReadAndRequiresExplicitApproveAll` | Inspect live Session detail |
| Task attach shows all Sessions and updates live | `TestTaskConsoleShowsEverySessionAndDetachesOnlyViewer`, `TestRefreshAddsNewTaskSessionWithoutRestart` | Interactive terminal smoke |
| Keyboard and mouse Session selection | `TestTaskConsoleKeyboardMouseAndExplicitPromptTarget` | Interactive terminal smoke |
| Direct Session attach: stored then live, no duplicate | `TestSessionConsoleReplaysThenDeduplicatesLiveAndTogglesRaw` | Interactive provider smoke |
| Busy steering is queued | `TestWorkerSerializesPromptsBroadcastsAndReplays` | Provider matrix |
| `Esc` cancellation and same-Session continuation | `TestEscapeCancelsActiveTurnAndPreservesTypedSteering`, worker tests | Interactive provider smoke |
| Detach does not interrupt turn | `TestAttachSessionProgramCanDetachWithoutWorkerMutation`, Task console test | Terminal/browser close smoke |
| Cursor reconnect recovers missed output | API test, `TestWorkerSerializesPromptsBroadcastsAndReplays`, console replay test | Provider disconnect/reconnect |
| Request/response correlation | adapter request test, worker test, lifecycle raw JSONL test, API test | Provider matrix |
| Normalized/raw Session logs | renderer tests, lifecycle test | Inspect live output |
| Aggregate Task logs | lifecycle test, `TestTaskHistoryUsesIndependentSessionCursors` | Multi-provider Task |
| Cancel Session | adapter, worker, console, and API tests | Provider matrix |
| Resume stopped Session | lifecycle test, service/API tests | Provider matrix |
| Persist distinct acpx/ACP/native IDs | store migration/round-trip, adapter, lifecycle tests | Provider metadata inspection |
| Associate and create Git worktrees | repository and service worktree tests, CLI test | Provider matrix branches |
| Exclusive provider-native mode | lifecycle fake-native test; service/native/worker tests | Conditional real Codex smoke |
| ACP/native never write concurrently | `TestNativeLeaseHoldsQueueUntilSuccessfulReconciliation`, native attach tests | Real native queue smoke |
| Reconcile before queued prompts continue | worker/native reconciliation tests, lifecycle test | Real native exit smoke |

## Review checklist

| Specification criterion | Deterministic evidence | Manual evidence |
| --- | --- | --- |
| Create reviewer Session from producer | lifecycle test, `TestReviewCreatesIndependentSessionWithCompleteEvidence`, API review route | CLI/web smoke |
| Reviewer reads producer JSONL | generated prompt assertion includes exact read-only path | Reviewer provider inspects file |
| Same repo/worktree | service and lifecycle assertions | Git inspection |
| Git branch/status/diff evidence | `TestCaptureReviewSnapshotIncludesBranchStatusAndDiffs`, review prompt assertions | Reviewer reruns Git commands |
| Independent verification instruction/checklist | review prompt exact assertions | Review output audit |
| Refuse changing producer by default | `TestReviewRejectsChangingProducerUnlessExplicit` | Active producer smoke |

## Web checklist

| Specification criterion | Deterministic evidence | Manual evidence |
| --- | --- | --- |
| `acpctl serve` starts local UI | `TestServeUsesLoopbackAndStopsOnlyTheViewer` | Open printed URL |
| Web lists Tasks and shows Task Sessions | `TestServerRenderedPagesAndVersionedBrowserBoundary` | Visual smoke |
| Clickable Session detail | server-render test asserts links/control page | Click smoke |
| Stored and live Session history | embedded EventSource client plus versioned API SSE replay test | Provider browser smoke |
| Web streams output | EventSource asset assertion and API SSE tests | Observe live turn |
| Prompt/cancel/resume/stop | versioned API route test plus browser client control bindings | Click controls |
| Review and sanitized raw execution view | API review test, rendered controls/asset assertions, raw-data redaction test | Click controls |
| Web shutdown leaves workers running | `TestStoppingWebServerLeavesSessionWorkerUntouched` proves unchanged PID/socket/status and zero runtime commands | Stop server during live turn |
| Browser hardening | DNS-rebinding/cross-origin test, CSP/escaping assertions | Inspect response headers |

## API/future-client checklist

| Specification criterion | Deterministic evidence |
| --- | --- |
| Stable Task/Session DTOs, not rows | API test checks every route and forbidden fields/values |
| Task stream aggregates all Sessions | API reconnect test plus `TestFollowTaskReplaysAndDiscoversNewSessions` |
| Session monotonic positions/request IDs | Session SSE assertions and cursor validation |
| Disconnected client loses no completed output | Session and component-wise Task `Last-Event-ID` replay assertions |
| CLI/TUI/API/web share services | compile-time `SessionStream` conformance; transports invoke `task.Service`/`SessionService` |
| Socket/raw ACP remain internal | API and web redaction assertions, sanitized activity test |
| Loopback-only and unsupported public exposure rejected | listener/API/CLI tests, Host/origin web tests |

## Persistence, privacy, and hardening

| Concern | Evidence |
| --- | --- |
| Only Task and Session tables | `TestFreshDatabaseHasOnlyTaskAndSessionTables`, `scripts/check-repository.sh` |
| Reopen/idempotent migration | store migration tests |
| Foreign keys/status/metadata | store and domain tests |
| Safe concurrent creation | store concurrent test, service repository-lock test, race suite |
| Private state/log/socket permissions | config, sessionlog, and worker tests |
| Duplicate/stale worker recovery | worker duplicate and reconciliation tests |
| No credentials/runtime artifacts tracked | `make security-check` |
| Build metadata is present | buildinfo unit test and deterministic binary version check |
| Clean static/build suite | format, all tests, race, vet, and real binary build in acceptance script |

## Authenticated live matrix

Deterministic fixtures prove product logic but cannot prove that a user's current provider installation is authenticated or that an external pre-1.0 adapter has not changed. The independent tester must run T08 in [TEST_PLAN.md](TEST_PLAN.md) for:

- Codex
- Claude
- Gemini
- Grok (`grok-build`)
- OpenCode

Each provider gets its own worktree under one acceptance Task, produces a harmless proof change, streams structured ACP history, survives viewer detach, handles a correlated follow-up, and stops/resumes the same Session. Unexpected login prompts are external-environment blockers, not silently skipped passes.

Provider-native live smoke is conditional on an exact native ID. Follow [CODEX_NATIVE_SMOKE.md](CODEX_NATIVE_SMOKE.md). If the ID is absent, record native attachment as unavailable for that adapter version; never create or conflate a second conversation.

## Release sign-off record

Record:

```text
tested SHA:
origin visibility:
make acceptance result:
gofmt -l . result:
go test ./... result:
go test -race ./... result:
go vet ./... result:
go build ./cmd/acpctl result:
provider matrix:
native smoke:
git status --short:
limitations/blockers:
```

The tester, not the implementation agent, owns independent sign-off and defect-loop gating.
