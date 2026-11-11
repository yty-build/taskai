# Local acceptance evidence

Acceptance runs write generated reports here. Raw reports and terminal captures are
ignored by Git; review and sanitize any evidence before deliberately publishing it.
A successful offline suite does not substitute for a live provider acceptance run.

For model-billed runs, follow the approval requirements in
[Contributing](../../CONTRIBUTING.md). Record the commit, provider versions, exact
scenarios, results, and cleanup outcome. Publish an approved, sanitized report with
`git add -f tests/evidence/<report>` and update the
[compatibility status](../../docs/compatibility.md) when its evidence changes.

Previous milestone reports are preserved in
[Git history](https://github.com/yty-build/taskai/tree/e1039c1802394682068bc9734860b5c1e496e038/tests/evidence).
