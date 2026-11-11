# Using TaskAI

TaskAI organizes native coding-agent sessions into tasks and teams. A Manager can
coordinate work with Subordinates; you can open and type into any agent's own CLI.
The agent's provider continues to handle authentication, models, tools, and approvals.

## Install and launch

Install Rust, `tmux`, and the provider CLIs you plan to use. Sign in through each
provider's normal CLI. TaskAI checks availability but does not install, upgrade,
or authenticate providers for you. Check [compatibility](compatibility.md) before
setting up messaging.

From the repository:

```sh
cargo build --release --locked
./target/release/taskai
```

To install the built binary into a directory on your `PATH`:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 target/release/taskai "$HOME/.local/bin/taskai"
taskai
```

Add `$HOME/.local/bin` to your shell's `PATH` if needed.

## Create your first team

1. Press `n` from Home, select **This computer**, and choose your repository.
2. Name the task, create it, and add a Manager using a provider you have configured.
3. From task navigation, use `a` to add a Subordinate under the selected Manager.
4. Type a request into an agent's native CLI. Use `Ctrl+G` to navigate between agents.

A local Subordinate can use the same repository, a separate repository, or a new Git
worktree. New worktrees default to the repository's current branch. Repository
browsing uses `F2`; `Space` chooses a directory, and `Tab` moves between fields.

## Everyday controls

These shortcuts apply while TaskAI navigation has focus. Inside an agent's CLI,
keystrokes go to that CLI except `Ctrl+G` and `Ctrl+F`.

| Key | Action |
|---|---|
| `Enter` | Open the selected task or agent |
| `n` | Create a task |
| `m` / `a` | Add a Manager / Subordinate |
| `Ctrl+G` | Return from the CLI to navigation |
| `Ctrl+F` | Toggle inline and full-screen CLI views |
| `t` | Open the task's ordinary shell |
| `l` | Open the task's message log |
| `s` | Open Settings |
| `F1` | Open the optional setup helper |
| `/` | Find a task |
| `G` / `Esc` | Go Home / go back or cancel |
| `?` | Show shortcut help |
| `q` | Leave TaskAI and keep sessions running |
| `d` / `D` | Open the scoped destruction confirmation |

Click a session to focus it. Scroll over the CLI to use its terminal history;
scrolling from navigation does not change focus. Forms support text selection,
copy/paste, arrow keys, `Ctrl+A`, and `Ctrl+U`.

## Let agents collaborate

Messaging connects TaskAI-launched agents in the same local task. Each has an address
such as `codex-lead` or `grok-reviewer` and can discover available teammates.

```sh
taskai doctor --workspace /absolute/path/to/your/repository
```

Review the result for each provider you intend to use. `doctor: green` requires all
three messaging providers to pass; one unavailable provider does not disable the
others. The command checks setup without sending a model prompt.

Add two supported agents. A new Codex session asks you to type its first message;
complete any native trust or login prompts, then enter a request such as:

> Ask the Grok reviewer in this task to review the last commit, and notify me when it is idle.

| Capability | Codex / Grok tool | Claude Code tool |
|---|---|---|
| Discover teammates and their status | `list_sessions` | `ListAgents` |
| Send work to a teammate | `send_message` | `SendMessage` |

Codex exposes these under `taskai`; Grok loads them through TaskAI's MCP server.
The send tools can request one idle notice with `notify_when_idle`.

The session rail shows activity and waiting states. Press `l` in task navigation to
inspect messages, delivery notices, and idle notices. Delivery can fail or remain
uncertain after a connection loss; an accepted message or an idle notice does not
prove that the requested work is correct. See [compatibility](compatibility.md) for
provider-specific behavior and the current verification boundary.

## Leave, return, or stop work

Each agent runs in a durable private tmux session. Leaving with `q` detaches TaskAI's
viewers; reopening TaskAI reconnects to the saved running sessions. If a provider
process exits, TaskAI reports that state rather than silently starting a replacement.

Use the destruction confirmation to stop a Subordinate, a Manager and its team, or a
whole task. The confirmation shows the affected scope. Repositories, worktrees,
branches, files, and provider-native conversation history are preserved.

The task's **Terminal** is an ordinary shell in its repository, separate from the
agents. It also survives leaving TaskAI. Type `exit` to close it; destroying the task
closes it with the task's agent sessions.

## Work on an SSH server

In **Settings → Servers**, add a display name, an SSH-config alias, and an optional
default repository directory. Select that server when creating a task. SSH keys or
an agent must already work without an interactive password prompt.

Install `tmux` and the desired provider CLIs on the remote host. Remote repository
paths belong to that host; `~` is resolved there. TaskAI stores the SSH alias and
uses your existing SSH configuration.

Server settings can select providers, executable paths, and default models. Changes
apply to new agents. `F5` refreshes readiness checks, `c` opens provider configuration,
and `t` opens a setup shell. A readiness result does not verify model quota or prove
that a model turn will succeed.

Remote Subordinates currently share the task's repository. Remote OpenCode and
cross-machine session messaging are not supported. TaskAI's local context bootstrap
is not installed on remote hosts automatically.

## Get setup help

Press `F1` and choose a local provider to open a dedicated helper task. Ask it about
setup or describe an objective to get a suggested team. It uses your provider account
and normal model billing.

The helper can queue forms for review. Use `Ctrl+G`, then `F8`, to open a queued form;
you review and submit it. Opening the helper does not automatically send a prompt,
install software, or change provider configuration.

## Context and scripts

Inside a locally managed agent session:

```sh
taskai context self
taskai context tree
taskai context session <ref>
```

These commands describe the agent's task, assignment, workspace, and team chain.
For structured output, use `--format json`.

From your own shell, `taskai session launch <ref>` starts or reconciles a registered
session without opening the UI. It is refused inside a managed agent session.

The older `taskai session prompt <ref> --stdin --wait` control command requires the
TaskAI UI and currently supports local Claude Code targets. It is separate from the
cross-provider messaging tools above.

State lives under `~/.taskai`, or the directory selected by `TASKAI_HOME`. Message
logs include message text. See [Security](../SECURITY.md) before using TaskAI with
sensitive repositories or untrusted agents.
