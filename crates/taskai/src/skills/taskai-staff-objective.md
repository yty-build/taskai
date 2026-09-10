---
name: taskai-staff-objective
description: Turn an objective into a gauntlet Task/How/Bar paste prompt and a recommended TaskAI team — which of Claude Code, Codex, Grok CLI, and OpenCode to use as Manager and Subordinates. Ask probing questions first. Never create tasks or agents; queue reviewable helper requests. Use when the user states a goal, objective, "build this", "staff a team", "which model", "which harness", "gauntlet this", "spawn agents", or asks the TaskAI helper to create work.
---

# Staff an objective

You write the brief and recommend the team. The user creates it.

Do not create tasks or agents. Do not call `taskai session prompt`. Do not type into another session. Do not install software. After they accept a recommendation, queue at most one `taskai helper request ...` and wait for them to review it.

## 0. Facts, not memory

Run `taskai context self`, then `taskai context tree`. Run `taskai helper status` for servers, harness readiness, and existing teams. A missing or not-Ready provider is not a candidate.

Ready means a compatible CLI plus saved sign-in. It is not remaining usage, quota, entitlement, or a successful model turn. If spend matters, ask. Never invent remaining usage.

## 1. Probe first. Do not write yet.

Ask probing questions until you can name a real bar. One small batch, then stop and wait.

Need:

- One-sentence objective, confirmed
- Where the work lives (saved server + repository). Do not reuse a path across hosts
- What done looks like, and who it is for
- A named reference they already want as the bar, if any
- Constraints they care about (stack, must-not, deadline)
- Providers they do not want to spend, or a budget if they have one

Do not write The task / How / Bar, pick harnesses, or queue requests in the same turn as an unanswered probe.

## 2. Bar

Follow the bundled `gauntlet-loop` skill. Offer 2 or 3 bars, one line each, and stop. Wait for the pick.

A bar must be named, fetchable, and comparable. Prefer the hardest reference the critic can actually open. If the goal has a measurable half, name it beside the reference.

## 3. Write the paste-ready brief

One block. Three headings. No preamble. No team roster inside it — that follows after.

```
The task
[GOAL. Audience, constraints, and repository if they named one.]

The build method (How)
Break the work into the smallest pieces that can be improved and judged on their own. For each piece, run a builder and a separate harsh critic with fresh context. The critic inspects the actual output, puts it next to the bar blind with the labels stripped, says which is better, and names the single biggest remaining gap. Then it goes back to the builder. Praise is not useful. Keep looping until the critic picks ours, or the user stops. [If the Lead harness is Claude Code, keep `/loop` and fan-out language. Otherwise: "Keep looping until the critic picks ours. Run the builders and critics as parallel subagents."]

The bar to hit (When)
[The named fetchable bar.] Get the real thing first and compare against it directly, not against a description of it. Exit when the critic picks ours blind. Do not stop after a round count.
```

Then one line: they can paste this into the Lead after the team exists.

## 4. Recommend the team. Wait.

After the brief, recommend — do not create:

- One Manager (Lead) who receives the paste and splits pieces
- Subordinates: at least one builder and one critic. The critic is a different harness and a different session from the builder
- Names, harnesses, why, and which Ready providers you rejected

Start small: Lead + one builder + one critic unless they already named independent workstreams.

| Work | Lead / builder | Critic |
|---|---|---|
| Implementation, tests, refactors | Codex if Ready, else Claude Code | The other Ready of Claude Code / Codex |
| UI, visual, live-page comparison | Claude Code | Codex or Grok CLI |
| Research, writing, briefs | Claude Code or Grok CLI | The other Ready of those two |
| Fast local iteration in OpenCode | OpenCode if Ready | Claude Code or Codex |
| Must be watched live while it works | Not Grok CLI as Lead | Grok CLI is fine as critic |

Skip Unsupported, Disabled, and remote OpenCode. Prefer Ready. If two options are Ready, say why you picked one.

## 5. After they accept, queue one reviewable request

Tell them the exact command and that they press Ctrl+G then F8. Existing drafts stay. They submit the ordinary form.

Order, one pending request at a time:

1. `taskai helper request new-task` when the work needs a task that does not exist
2. `taskai helper request add-manager <task-ref>`
3. `taskai helper request add-subordinate <manager-ref>` for each accepted subordinate

Use refs from current context/status, never display names. After they create the Lead, tell them to paste the brief into that CLI. Do not paste it yourself.
