# TaskAI

TaskAI lets Claude Code, Codex, and Grok collaborate through a lightweight mailbox.
Agents discover teammates and send messages into each other's sessions without typing into terminal windows.
You keep using each agent's native CLI, with human input handled separately from agent messages.
Follow their work in the terminal and inspect message delivery in TaskAI, while keeping the providers' own approval controls and conversation history.

## Get started

You need Rust, `tmux`, and the provider CLIs you want to use, installed and signed in.

```sh
git clone https://github.com/yty-build/taskai.git
cd taskai
cargo build --release --locked
./target/release/taskai
```

Create a task with `n`, choose a repository, and add your agents.
Use `Ctrl+G` to return from an agent's CLI to task navigation; `q` there leaves
TaskAI while the sessions keep running.

Messaging currently works between supported local Claude Code, Codex, and Grok
sessions in the same task. OpenCode is available as a terminal session.
See [compatibility](docs/compatibility.md) for tested versions, setup requirements,
and verification limits.

## Learn more

- [User guide](docs/user-guide.md) — teams, shortcuts, messaging, and SSH tasks.
- [Architecture](docs/architecture.md) — sessions, adapters, and task context.
- [Contributing](CONTRIBUTING.md) — source layout, builds, and tests.
- [Security](SECURITY.md) — permissions, local state, and reporting vulnerabilities.

## License

[Apache License 2.0](LICENSE).
