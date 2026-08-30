# taskai

**One terminal. Many tasks. Teams of native CLI agents working in parallel.**

TaskAI is the command center around agent CLIs. It organizes work as `Task → Manager → Subordinate` while every agent remains inside its real native harness.

## The non-negotiable surface

TaskAI owns the task board, hierarchy, status, creation flows, and navigation chrome. The entire working pane belongs to the selected session's actual native CLI.
Harnesses can include Claude Code, Codex, Grok CLI, OpenCode, and any other compatible CLI. Managers and subordinates follow the same rule.
The CLI owns its input, output, tools, approvals, colors, scrollback, and interaction model. TaskAI never substitutes a chat, composer, transcript, or tool renderer.
`Ctrl+G` is the one reserved boundary chord that leaves the CLI and returns focus to TaskAI navigation. While the CLI is focused, every other keystroke passes through unchanged.
Single-letter TaskAI commands work only while navigation has focus, so typing `a`, `m`, `d`, or ordinary prose inside a CLI can never trigger TaskAI actions.

## The complete journey

### 1. Launch into the task board

Running `taskai` always opens the portfolio, not an arbitrary setup screen or stale terminal.

```text
┌─ Tasks ─────────────────────────────────────────────────────────────┐
│ Harnesses [h]  Claude Code ✓·2  Codex ✓·3  Grok CLI !  OpenCode ✓·1│
│                                                                    │
│ ● Repository evaluation     Managers: Claude Code, Codex · Working│
│ ! Production release        Manager: OpenCode · Needs attention   │
│ ✓ API migration             Managers: Grok CLI, Codex · Complete  │
│                                                                    │
│                         + New task                                 │
├────────────────────────────────────────────────────────────────────┤
│ [Enter] Open  [n] New task  [h] Harnesses  [/] Find  [?] Help     │
└────────────────────────────────────────────────────────────────────┘
```

Every row summarizes a whole team. A user can see what is active, blocked, waiting, or complete before opening anything.
The Harnesses strip is always visible on Home. `✓` means ready, `!` means setup is required, and the number after `·` is the count of live sessions using that CLI.

`h` opens native harness settings:

```text
┌─ Native CLI harnesses ──────────────────────────────────────────────┐
│ Claude Code   ✓ Ready          2 live   command: claude            │
│ Codex         ✓ Ready          3 live   command: codex             │
│ Grok CLI      ! Needs setup    0 live   command: —                 │
│ OpenCode      ✓ Ready          1 live   command: opencode          │
├────────────────────────────────────────────────────────────────────┤
│ [Enter] Configure  [r] Recheck  [+] Add custom CLI  [Esc] Home    │
└────────────────────────────────────────────────────────────────────┘
```

This screen shows what TaskAI can launch and what is currently running. Executable or custom commands are configured here once, then selected by name everywhere else.

### 2. Add a Task

From Home, `n` opens Task creation. The only typed field is the Task name; the repository is chosen with an explorer.

```text
┌─ New Task ──────────────────────────────────────────────────────────┐
│ Name        Repository evaluation                                  │
│ Repository  /Users/me/Documents/GitHub/buzz                        │
│             [Browse…]                                              │
│                                                                    │
│                              [Create Task]                          │
├────────────────────────────────────────────────────────────────────┤
│ [↑/↓] Move  [Enter] Select  [b] Browse  [Esc] Cancel              │
└────────────────────────────────────────────────────────────────────┘

┌─ Choose repository ─────────────────────────────────────────────────┐
│ Current path  /Users/me/Documents/GitHub                            │
│                                                                    │
│  ../                                                               │
│  ▸ acp-gateway/                                                     │
│  ▸ buzz/                 ← selected repository                     │
│  ▸ another-project/                                                │
├────────────────────────────────────────────────────────────────────┤
│ [↑/↓] Move  [→/Enter] Open  [←] Parent  [Space] Choose  [/] Find │
└────────────────────────────────────────────────────────────────────┘
```

The explorer starts at the current directory and remembers recent repositories. It shows the canonical path at all times, so users navigate instead of copying and pasting paths.
After Create, the Task exists with no agents yet:

```text
┌─ Repository evaluation ──────────┬─ No session attached ───────────┐
│ /Users/me/Documents/GitHub/buzz  │                                  │
│                                  │ Add the first Manager to begin.  │
│ No Managers                      │                                  │
│                                  │                                  │
│        + Add first Manager       │                                  │
├──────────────────────────────────┴──────────────────────────────────┤
│ [m] Add Manager  [e] Browse repository  [d] Destroy  [T] Home     │
└─────────────────────────────────────────────────────────────────────┘
```

### 3. Add the first Manager

Pressing `m` opens a Manager drawer. Harness is an arrow selector populated only from configured, ready harnesses; it is never a command text field.

```text
┌─ Add Manager ───────────────────────────────────────────────────────┐
│ Name         Lead                                                   │
│ Harness      ‹ Claude Code ›      [✓ Ready]                        │
│ Session      ‹ New ›                                                │
│ Model        ‹ Opus ›             shown only when supported         │
│ Effort       ‹ Medium ›           shown only when supported         │
│ Permissions  ‹ Ask ›              shown only when supported         │
│                                                                    │
│                              [Create Manager]                       │
├────────────────────────────────────────────────────────────────────┤
│ [↑/↓] Field  [←/→] Choose  [Enter] Create  [Esc] Cancel          │
└─────────────────────────────────────────────────────────────────────┘
```

Custom CLI commands are added from Home → Harnesses, never during Task or agent creation. Unsupported model, effort, or permission fields are omitted rather than guessed.

### 4. The Manager opens in its native CLI

Create starts the Manager and immediately attaches its exact native harness:

```text
┌─ Repository evaluation ──────────┬─ Lead · Claude Code · ● Running ┐
│ ● Lead · Claude Code             │                                  │
│                                  │ [exact live Claude Code CLI]     │
│ No Subordinates                  │                                  │
│                                  │ TaskAI does not redraw, parse,   │
│                                  │ or replace anything in this pane.│
├──────────────────────────────────┴──────────────────────────────────┤
│ [Ctrl+G] TaskAI navigation                         CLI has focus    │
└─────────────────────────────────────────────────────────────────────┘
```

The pane title always names the selected session, harness, and state. The focused pane has the highlighted border: navigation focus shows TaskAI commands, while CLI focus shows only the `Ctrl+G` escape chord.

### 5. Add a Subordinate to that Manager

If the CLI has focus, press `Ctrl+G` once to return to navigation. Move the arrow highlight onto Lead and press `a` directly.
`a` means Add Subordinate whenever a Manager is highlighted; TaskAI already knows the parent, so there is no separate parent-selection step. The Manager remains alive while the compact creation drawer opens.

```text
┌─ Add Subordinate ───────────────────────────────────────────────────┐
│ Parent       Lead · Claude Code                                    │
│ Name         Architecture                                          │
│ Harness      ‹ OpenCode ›         [✓ Ready]                        │
│ Session      ‹ New ›                                                │
│ Workspace    ‹ Same repository as Manager ›                        │
│ Model        ‹ Default ›          harness-supported choices only   │
│                                                                    │
│                           [Create Subordinate]                      │
├────────────────────────────────────────────────────────────────────┤
│ [↑/↓] Field  [←/→] Choose  [Enter] Create  [Esc] Cancel          │
└─────────────────────────────────────────────────────────────────────┘
```

After Create, the tree gains the child and its native CLI is attached:

```text
│ ● Lead · Claude Code             │ Architecture · OpenCode · ● Run │
│ └─ ● Architecture · OpenCode     │ [exact live OpenCode CLI]       │
```

Workspace offers three arrow-selected choices: the Manager's repository, another repository, or a new git worktree.
Another repository opens the same directory explorer used by Task creation. New worktree asks for base repository, base branch, and worktree name, previews the resulting path, then creates it before launching the Subordinate.
When creation is confirmed, TaskAI creates the Subordinate session under the highlighted Manager in the chosen workspace and attaches its native CLI. The user can return to navigation, select Lead, and press `Enter` to reattach the still-running Manager CLI.
Managers may also create Subordinates through orchestration; those sessions appear under the correct parent in the same tree.

### 6. Destroy a Task, Manager, or Subordinate

From navigation, select an item and press `d`. TaskAI shows the exact blast radius before anything stops.

```text
┌─ Destroy Lead and its sessions? ────────────────────────────────────┐
│ This removes from TaskAI:                                          │
│   Manager session · Lead · Claude Code                              │
│   Subordinate session · Architecture · OpenCode                     │
│                                                                    │
│ Repository: /Users/me/Documents/GitHub/buzz                         │
│ The repository is never changed or deleted by Destroy.             │
│ Type Lead to confirm:  __________                                  │
├────────────────────────────────────────────────────────────────────┤
│ [Enter] Destroy  [Esc] Cancel                                      │
└─────────────────────────────────────────────────────────────────────┘
```

Destroying a Subordinate stops and removes only that Subordinate session. Destroying a Manager stops and removes its session plus every Subordinate session beneath it.
Destroying a Task stops and removes all Manager and Subordinate sessions, then removes only the TaskAI Task entry.
The repository path is displayed in every confirmation, but repository deletion is not an available action. Destroy never removes source files, directories, git data, or CLI-owned native history.
A TaskAI-created worktree is also retained by Destroy. Worktree cleanup is a separate explicit action with its own path and branch confirmation.

### 7. Add a second Manager to the same Task

Return to navigation and press `m` anywhere at the Task root. Choose a configured harness with arrows and create it exactly like the first Manager.

```text
┌─ Repository evaluation ──────────┬─ Delivery · Codex · ● Running ─┐
│ ● Lead · Claude Code             │                                  │
│ └─ ● Architecture · OpenCode     │ [exact live Codex CLI]          │
│ ● Delivery · Codex               │                                  │
│                                  │                                  │
├──────────────────────────────────┴──────────────────────────────────┤
│ [Enter] Attach  [m] Add Manager  [a] Add Subordinate  [d] Destroy │
└─────────────────────────────────────────────────────────────────────┘
```

The new Manager is a peer of Lead, not its child. Creating it does not interrupt Lead or Architecture.

### 8. Add a Subordinate to the second Manager

Select Delivery, press `a`, and the drawer identifies `Delivery · Codex` as Parent. Choose a configured harness and same repository, another repository, or new worktree with arrows, then create.

```text
┌─ Repository evaluation ──────────┬─ Verification · Grok CLI · ● Run ┐
│ ● Lead · Claude Code             │                                  │
│ └─ ● Architecture · OpenCode     │ [exact live Grok CLI]           │
│ ● Delivery · Codex               │                                  │
│ └─ ● Verification · Grok CLI     │                                  │
├──────────────────────────────────┴──────────────────────────────────┤
│ [Enter] Attach  [m] Add Manager  [a] Add Subordinate  [T] Home    │
└─────────────────────────────────────────────────────────────────────┘
```

Parent is always shown before confirmation, preventing a Subordinate from being added to the wrong Manager.

### 9. Deploy and supervise multiple Tasks

`T` returns Home without stopping the current team. The user creates another Task with `n` and repeats the same visible lifecycle.
Task rows aggregate team state; expanding a Task reveals Managers, and expanding a Manager reveals Subordinates. Every row includes its harness.
Attention rises through the hierarchy: a waiting Subordinate marks its Manager, and a waiting Manager marks its Task. Selecting it attaches that exact native CLI.

### 10. Leave and resume

Leaving TaskAI detaches the viewer but does not stop Tasks, Managers, or Subordinates. They continue independently and preserve their native sessions.
Launching `taskai` again returns to the task board with the same hierarchy and current status. `Enter` reattaches the exact native CLI where the user left it.

TaskAI should feel like leading several teams from one calm portfolio—not operating a maze of terminals.
