# TaskAI

TaskAI brings your coding agents together in one terminal, so you can work on several tasks without juggling windows.
Keep using Claude Code, Codex, Grok CLI, or OpenCode with their familiar interfaces and approval controls.
Claude Code, Codex, and Grok agents can find teammates and exchange work through native session integrations, with shared task context that makes each agent's role clear.
Sessions keep running when you close TaskAI, so you can return to the same team and pick up where you left off.

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
