# Security

## Reporting a vulnerability

Please report privately through GitHub's "Report a vulnerability" flow on this
repository's Security tab rather than opening a public issue. Include what you did, what
happened, and the versions of TaskAI and the provider CLI involved.

## What TaskAI does on your machine

TaskAI is a local tool with a broad footprint, so it's worth being explicit:

- **It spawns provider CLIs as child processes** in real ptys, in a workspace you choose,
  with your environment. Those processes have whatever access your shell has.
- **It writes into the workspaces you point it at**: a managed block in `AGENTS.md` (with
  a `CLAUDE.md` symlink) and hook entries in `.claude/settings.local.json`. Both merge
  into existing files and never overwrite your content.
- **It opens a unix domain socket** at `~/.taskai/run/control.sock`, created `0600` inside
  a `0700` directory, used by `taskai session prompt` and `taskai session cancel`.
- **It stores state** in `~/.taskai` (`0700`), overridable with `TASKAI_HOME`.

## What it deliberately does not do

- Credentials never enter argv, prompts, state files, or logs. Provider authentication
  stays entirely inside the provider CLIs.
- The dispatch log records correlation only — source, target, event, timestamp — never
  prompt or response content.
- Destroy removes TaskAI's own records and processes. It never deletes repositories,
  worktrees, git history, or provider-native conversation history.
- TaskAI never originates a turn on its own; every turn is initiated by a human or by a
  registered session.

## Trust boundary

The socket and runtime directory are owner-only, which protects them from *other users*
on the machine. They are **not** a boundary against another process running as you — such
a process can reach the socket and drive attached sessions. Treat TaskAI as you would
treat your shell.

Because TaskAI launches agent CLIs that can read and write your files, only point it at
repositories you trust, and keep the provider CLIs' own approval settings configured the
way you want them.
