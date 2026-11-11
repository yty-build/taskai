# Architecture

TaskAI owns tasks, team context, session references, terminal viewers, and messaging
adapters. Provider CLIs own their conversations, model behavior, tools, and approvals.
Humans and agents decide what work to do; TaskAI routes their requests.

## Tasks and sessions

A task groups Manager and Subordinate sessions around a repository and a local or
SSH connection. Roles describe the team's structure; they are not a permission
boundary. Each session has a stable TaskAI reference, workspace, provider mapping,
runtime locator, and recorded state.

Provider session identity and tmux runtime identity are stored separately. Reattaching
a viewer must not create another provider conversation or silently replace a process.
The registry persists under `~/.taskai/state`; `TASKAI_HOME` overrides the root.

Each agent runs in a private tmux session. Codex and Grok messaging bridges, and
Claude's observer, run alongside their agent. The tmux server preserves the processes
when the UI exits. TaskAI has no separate central resident daemon.

A task's utility terminal is a shell, not an agent. Destroying a task removes its
owned runtimes and registry records while preserving repositories, worktrees, and
provider history.

## Context

`taskai context self`, `tree`, and `session <ref>` expose task identity, assignment,
workspace, status, and team relationships. Text and JSON views derive from the same
registry. Snapshots are revisioned; context describes the environment without
assigning new work or approving actions.

Local sessions receive `TASKAI_SESSION_REF`. TaskAI installs a managed `AGENTS.md`
bootstrap and supported provider hooks so agents can retrieve their context.
Existing user-owned instructions are preserved. See [taskai-context](../crates/taskai-context/src/lib.rs).

## Agent-to-agent messaging

Each participating session has an address in the task's registry. A new harness needs
an adapter with a way to deliver into its exact live session, a way for the agent to
call messaging tools, and lifecycle/status observation. MCP can supply the tool
interface; receiving into a live provider session is a separate integration.

| Provider | Agent sends through | Agent receives through |
|---|---|---|
| Codex | `taskai.list_sessions` and `taskai.send_message` dynamic tools | Shared app-server `turn/start` on the owned thread; may steer a running turn |
| Grok | `list_sessions` and `send_message` through TaskAI's MCP server | A second client issuing `session/prompt` to the session's leader |
| Claude Code | Native `ListAgents` and `SendMessage` | Native peer socket and envelope |

Codex tools are registered at `thread/start`; its bridge answers `item/tool/call`.
Grok's MCP process forwards calls over a private bridge socket using the session's
identity. Claude integration mirrors TaskAI sessions into its native peer registry.
There is no dependency on a Claude session being present for Codex and Grok to message.

An outbound tool call resolves a same-task recipient, passes a message through TaskAI's
mailbox transport, and reaches the recipient's provider adapter. Agent-to-agent message
bodies are not typed into terminal panes. OpenCode retains its terminal integration
and has no adapter for this messaging path.

The mailbox uses authenticated local Unix sockets with newline-delimited JSON frames.
Messages carry an ID, sender identity and permission mode, recipient, priority, body,
and hop chain. Receipts and one-shot idle subscriptions preserve correlation with the
sender's own requests. HMAC hop tokens preserve chain provenance; provider-native loop
limits remain the provider's responsibility.

Normal delivery does not produce a universal success acknowledgement. Adapters track
provider-specific acceptance and failure; an uncertain write must not be treated as a
safe retry. An idle notice reports session activity, not the correctness of its work.

## Native interaction and legacy control

Human keyboard input goes directly to the focused native CLI. The provider remains
responsible for questions, approvals, and tool execution. TaskAI must not answer
approvals or change the session's declared permission mode.

The older UI control socket supports `taskai session prompt` and `session cancel`.
Prompt submission currently targets local Claude Code sessions through their owned
terminal. This is distinct from the native peer/mailbox messaging path above;
provider connectors also retain their existing ACP-related functionality.

## Recovery and compatibility

Registry records bind TaskAI references to exact provider session IDs and runtime
locators. Bridges validate those bindings before attaching or delivering. Closing
a viewer is a detach; destruction is scoped to explicitly selected TaskAI runtimes.

Each messaging adapter has independent version and protocol gates. A failed check
disables that adapter and reports a reason. Native provider responses, disconnected
bridges, pending startup, and uncertain delivery must remain distinct states.

See [compatibility](compatibility.md) for tested versions, platform restrictions,
startup constraints, and the current live-verification gap.

## Source map

| Crate | Responsibility |
|---|---|
| `taskai-core` | Registry, tasks, sessions, mailboxes, and workspace provisioning |
| `taskai-context` | Context snapshots and bootstrap installation |
| `taskai-connectors` | Provider adapters, tool interfaces, and legacy control |
| `taskai-pane` | PTY I/O and terminal grid |
| `taskai-tui` | Rendering, focus, input, and overlays |
| `taskai` | Application, lifecycle, bridges, settings, helper, and CLI commands |

Sockets and keys are owner-only, but this is a same-user trust model rather than a
sandbox. Message logs contain message bodies. See [Security](../SECURITY.md).

Earlier design decisions, prototype code, and acceptance plans remain available in
[Git history](https://github.com/yty-build/taskai/tree/e1039c1802394682068bc9734860b5c1e496e038).
