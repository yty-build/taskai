#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$repository_root"

check_forbidden_path() {
  path=$1
  case "$path" in
    .env.example)
      return 0
      ;;
    .env|.env.*|.agent/*|.codex/*|.claude/*|.gemini/*|.opencode/*|bin/*|worktrees/*|agent|\
    *.db|*.db-*|*.sqlite|*.sqlite3|*.sock|*.pid|*.log|*.jsonl|*.pem|*.key|*.p12|*.test|*.out|coverage.*)
      printf 'forbidden tracked runtime, credential, or build file: %s\n' "$path" >&2
      return 1
      ;;
  esac
}

git ls-files | while IFS= read -r path; do
  check_forbidden_path "$path"
done

git log --all --name-only --format= | sed '/^$/d' | sort -u | while IFS= read -r path; do
  check_forbidden_path "$path"
done

secret_pattern='-----BEGIN (RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|(sk|rk)_live_[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{20,}|AIza[0-9A-Za-z_-]{35}'
if matches=$(git grep -nI -E -- "$secret_pattern"); then
  printf 'possible credential material found in tracked files:\n%s\n' "$matches" >&2
  exit 1
fi

for revision in $(git rev-list --all); do
  if matches=$(git grep -nI -E -- "$secret_pattern" "$revision"); then
    printf 'possible credential material found in revision %s:\n%s\n' "$revision" "$matches" >&2
    exit 1
  fi
done

for ignored_path in \
  .agent/agent.db \
  .codex/auth.json \
  .claude/auth.json \
  bin/acpctl \
  coverage.out \
  session.jsonl \
  worker.log \
  worker.sock
do
  if ! git check-ignore -q -- "$ignored_path"; then
    printf 'expected runtime path is not ignored: %s\n' "$ignored_path" >&2
    exit 1
  fi
done

actual_tables=$(sed -n 's/^CREATE TABLE \([a-z_][a-z_]*\).*/\1/p' internal/store/migrations/*.sql)
expected_tables=$(printf 'tasks\nsessions')
if [ "$actual_tables" != "$expected_tables" ]; then
  printf 'persistent entity table set changed; expected only tasks and sessions, got:\n%s\n' "$actual_tables" >&2
  exit 1
fi

for fixture in scripts/acceptance.sh scripts/check-repository.sh internal/acp/testdata/fake-acpx internal/native/testdata/fake-native; do
  if [ ! -x "$fixture" ]; then
    printf 'deterministic fixture is not executable: %s\n' "$fixture" >&2
    exit 1
  fi
done

printf 'repository safety checks passed\n'
