# Compatibility

## Provider support

TaskAI uses the provider CLIs installed on your machine. Terminal launch support and
agent-to-agent messaging have separate compatibility checks.

| Provider | Terminal launch minimum | Messaging adapter tested against |
|---|---|---|
| Claude Code | 2.0 | 2.1.322; currently restricted to the verified macOS PID domain |
| Codex | 0.100 | Shared app-server daemon 0.167.1 |
| Grok CLI | 1.0 | 1.0.77 |
| OpenCode | 1.18 | No messaging adapter |

The application uses Unix process and socket facilities and requires `tmux`. The
version checks in the source determine actual availability; a terminal launching
successfully does not establish messaging support.

Run `taskai doctor --workspace /absolute/repository/path` to check messaging setup,
or inspect each provider in Settings. The global `doctor: green` result requires
all three messaging providers; an individual adapter can work while another is
unavailable. Checks do not submit a model prompt. Saved sign-in does not prove model
access, quota, or a successful model turn.

Messaging adapters are gated to their tested versions. If a provider changes, its
adapter stays off until the integration is re-verified. The other adapters continue
independently. Version constants live in [doctor.rs](../crates/taskai/src/doctor.rs)
and [Claude's drift checks](../crates/taskai-connectors/src/claude/drift.rs).

## Messaging boundaries

- Only sessions launched by TaskAI on the same computer, in the same task, participate.
- Codex messaging needs the first human message to initialize its visible thread.
  If its bridge stops before that message, destroy and recreate that agent.
- Starting `/new` inside Codex creates a thread without TaskAI's sending tools.
- Grok must trust the workspace before it loads the workspace MCP integration.
- Approvals remain in each provider's native CLI. TaskAI does not approve actions or
  change a session's permission mode.
- Claude can hold messages from a different permission mode for human review and
  applies its own loop and rate limits. These are Claude-specific behaviors.
- TaskAI preserves uncertain delivery after a connection loss rather than assuming
  success or blindly sending the same work again. Delivery acceptance and an idle
  notice do not guarantee a correct result.

SSH task sessions are available with the remote provider and tmux installed.
Cross-machine messaging, remote worktree creation, and remote OpenCode are not
currently supported.

## Verification status

Automated tests cover the mailbox, registry, adapters, lifecycle, readiness checks,
and terminal interface. Tests that need an absent or unsupported provider may skip;
set `TASKAI_REQUIRE_HARNESSES=1` when those integrations must be present.

The retained [zero-prompt rehearsal](https://github.com/yty-build/taskai/blob/e1039c1802394682068bc9734860b5c1e496e038/tests/evidence/session-messaging-m9-dry-run-2026-11-11.json)
exercised installed-provider setup, registration, and selected lifecycle paths.
Live message exchanges were demonstrated on a prototype. The current mailbox-based
implementation's billed end-to-end acceptance test has not been run; this remains
an explicit release-validation gap.

The runnable acceptance test is [release_messaging.rs](../crates/taskai/tests/release_messaging.rs).
Follow the approval and evidence requirements in [Contributing](../CONTRIBUTING.md)
before running tests that make model calls. Old development captures and milestone
reports remain in [Git history](https://github.com/yty-build/taskai/tree/e1039c1802394682068bc9734860b5c1e496e038/tests/evidence).
