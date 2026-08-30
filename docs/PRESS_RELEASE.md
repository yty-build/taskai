# TaskAI: The Day You Stop Being the Typist

*An open-source terminal command center that runs teams of native CLI coding agents — Claude Code, Codex, Grok CLI, OpenCode — and tells you what's working, what's blocked, and what needs you, before you open a single pane.*

**SAN FRANCISCO — August 30, 2026** — Today the TaskAI project released TaskAI, a terminal command center that organizes native CLI coding agents into teams — Task → Manager → Subordinate — while every agent keeps running inside its actual, unmodified CLI. Ask any agent on a TaskAI team where it fits, and it answers from inside its real harness:

```text
You are Subordinate S2, created to evaluate the repository's authentication flow.
You contribute evidence to Task T1: "Assess release readiness."
Your team chain is Task T1 → Manager M1 → you; M1 is active in /repo/main.
Peer S1 is testing the API in /repo/api and can be reached as session S1.
Your workspace is /repo/auth-worktree; report findings to M1.
```

That is the output of `taskai context self`, and it is the reason TaskAI exists.

Here is how most developers run multiple coding agents today: four terminal panes, a mental map of which pane is doing what, and a job that amounts to being a human message bus. One agent is waiting on an approval you haven't seen. One finished twenty minutes ago. One compacted its context and forgot its assignment mid-task, so you re-paste the brief. You are the only component in the system that knows the org chart. You aren't leading this work. You're typing for it.

Every tool that promised to fix this made the same trade. One camp wraps agents in its own chat UI: re-rendered transcripts, rebuilt approval prompts, the vendor's features re-shipped on a permanent lag. The other camp hands you raw terminal splits: full fidelity, zero structure. You lose the real CLI, or you lose your mind.

TaskAI refuses the trade. The board is TaskAI's. The pane is the CLI's. Running `taskai` opens a portfolio — every Task on one screen, each row summarizing a whole team: working, needs attention, complete. Attention rises through the hierarchy: a waiting Subordinate marks its Manager, a waiting Manager marks its Task. You read one screen, see the single thing that needs you, press Enter, and land inside that agent's actual native CLI — its colors, scrollback, keybindings, tool output, and approval prompts, byte for byte. Handle it, go back to the board. That's the whole loop. It's the tech-lead loop, not the typist loop.

## What you actually get, per CLI

TaskAI publishes a proven integration mode for each harness and never claims more than it can prove:

| Harness | Mode | What you actually get |
|---|---|---|
| Codex | `shared-live` | The same session updates live |
| OpenCode | `shared-live` | The same session updates live |
| Claude Code | `owned-tui` | Work renders live in the one connected native process |
| Grok CLI | `handoff-resume` | "Updating via ACP," then an exact-ID resume — not live |

If identity or safety checks fail, the mode is reported as `unsupported`. TaskAI does not fake "live."

## How it works

**Four keys build a team.** `n` creates a Task — type a name, pick the repository with an arrow-key explorer. `m` adds a Manager; `a` adds a Subordinate under whichever Manager you've highlighted, with the parent shown before you confirm. `d` destroys — after showing you exactly what stops. Harness, session, model, and permissions are arrow selectors populated only with what your CLIs actually support; unsupported fields are omitted, never guessed.

**One reserved chord.** `Ctrl+G` moves focus between the CLI and TaskAI navigation. That is the entire keybinding tax. While a CLI has focus, every other keystroke passes through unchanged — typing `a`, `m`, or `d` into your agent can never trigger a TaskAI action.

**Agents know who they are.** `taskai context self` answers: who am I, why was I created, what is my assignment. `taskai context tree` shows the whole team chain; `context session <ref>` explains a teammate's role, workspace, and state. Context is revisioned, refreshed on resume and compaction, and bound to the registered process — not to whatever identity a model claims. Context informs. It never decides.

**Teams can talk.** Any registered session can send a prompt to another through the same attributed conduit you use. TaskAI resolves the exact provider session and transports the intent. It does not rank targets, inspect prompt meaning, or approve delegation.

**Workspaces without ceremony.** A Subordinate can share its Manager's repository, use another one, or get a fresh git worktree — TaskAI previews the exact path before creating it.

**Leave and resume.** Quitting TaskAI detaches the viewer; every agent keeps working. Relaunch, press Enter, and you're back in the exact session where you left it.

"Every wrapper starts with good intentions and ends up re-implementing someone else's CLI on a two-week lag," said Trinath Y., TaskAI's creator. "The only way to never drift from the real thing is to never copy it. TaskAI has no transcript, no chat view, no tool renderer — the pane is the process. And when we can't do live safely, we label it handoff-resume. We'd rather ship a truthful limitation than a convincing fake."

"I ran my usual gauntlet: custom keybindings, an approval prompt mid-turn, a subordinate coming back from compaction," said Dana Okafor, a staff engineer at a fintech company who tested TaskAI for six weeks. "The chord is the one thing it takes, like screen's prefix — everything else hits the real CLI. The compacted agent ran `context self` and picked up its assignment without me re-pasting a brief. But what actually sold me was duller than that: I stopped keeping a text file of which pane was doing what. And when the table labeled Grok handoff-resume instead of pretending it was live, I trusted the rest of it."

"I'd never touched tmux and I assumed that disqualified me," said Marcus Leung, a developer two years into his career. "There's nothing to learn — arrow keys, Enter, one chord. I gave the Manager the goal and it briefed its own subordinates; my job turned into reviewing work instead of babysitting terminals. And the first time I pressed `d`, it listed exactly what would stop and told me my repo wasn't touchable. That's when I relaxed."

## Availability

TaskAI is open source under the MIT license and available today:

```bash
npm install -g taskai
taskai
```

A clean install is TaskAI plus the provider CLIs you already have — no tmux, no local model server, no daemons to babysit. Press `h` to register your CLIs once; select them by name everywhere after.

Start one Task, add one Manager, and ask it `taskai context self`.

Your agents are already excellent in their own terminals. Stop typing for them. Start leading them: **github.com/taskai/taskai**

## FAQ

**Isn't this just another wrapper?**
No, and the difference is mechanical, not cosmetic. A wrapper re-renders your agent's output inside its own UI and chases every upstream change to prompts, markdown, and approval flows. TaskAI attaches the running native process to the working pane, and there is no TaskAI chat view, composer, tool renderer, or transcript store anywhere in the product. Nothing can drift out of sync with your agent, because TaskAI keeps no copy of your agent.

**How is this different from tmux splits?**
tmux gives you panes. TaskAI gives every agent an identity, an assignment, and a team chain it can query (`taskai context self`, `context tree`), truthful status that rolls up to one board, and an attributed conduit for sending prompts between sessions instead of you copy-pasting between panes. Also: production TaskAI has no tmux dependency at all.

**What does TaskAI deliberately not do?**
Plenty, and we're proud of the list: no turns of its own, no delegation policy or agent ACLs, no task planning or target selection, no schedulers, no central approval handling, no prompt interpretation, no semantic result aggregation, no provider-neutral transcript bus, no automatic restart or retry. These are written into the design as excluded expansions that can't be smuggled back in as "safety features." Your Managers decide who works on what. TaskAI carries intent; it never invents it.

**Will agents spawn agents and burn my credits overnight?**
TaskAI never originates a turn. Every prompt traces to you or to a registered session you created — no scheduler, no automatic retries. Even capability testing is guarded: harmless handshakes run automatically, but any probe that creates a session or bills a model requires your explicit approval first.

**What happens when I close my laptop?**
Leaving TaskAI detaches the viewer and stops nothing; relaunch and the same board is there, and Enter reattaches each exact session. Honestly, though: OS sleep is the OS's business. If your machine suspends, processes suspend — that's true of every terminal tool, and we won't pretend otherwise.

**What if a CLI can't support live updates?**
Then TaskAI says so. Grok CLI is labeled `handoff-resume`: you see "Updating via ACP," competing input is suspended, and the native CLI resumes on its exact session ID afterward. If safe handoff or exact resume can't be proven, the harness is reported `unsupported` — never downgraded to a vague "should work."

**Does destroy or uninstall touch my repo?**
Never. Destroy removes TaskAI sessions and entries only — the whole list is shown before you type the confirmation. Source files, git data, provider-native history, and even TaskAI-created worktrees all survive; worktree cleanup is a separate action with its own path-and-branch confirmation. Delete TaskAI entirely tomorrow and you lose an org chart, not your work.

**Does TaskAI modify my CLI installs, config, or approvals?**
Provider CLIs stay user-installed and are never modified or silently upgraded. The context bootstrap is a small managed block in `AGENTS.md` that never silently overwrites an existing instruction file. Approvals happen in the native CLI, uninterrupted. Exactly one keystroke — `Ctrl+G` — is reserved.

**Does the context service tell my agents what to do?**
No, and this is the line we will not cross. Context is descriptive fact: who you are, why you exist, who is around you. What an agent does with that knowledge is up to the agent — or its Manager. The one guard TaskAI does keep is mechanical: a connector refuses technically unsafe concurrent writes into a provider session. That's collision protection, not a permission system.
