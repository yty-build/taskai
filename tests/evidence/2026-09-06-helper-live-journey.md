# Optional helper: isolated live journey — 2026-09-06

Tested 22:18–22:22 PDT in a private tmux session, with the real debug TaskAI UI
and explicitly fake providers. Debug binary SHA-256:
`2a984de91d49329533095f2569be6621732f59d4e561005c7821dd62bb340245`.

Isolation: `/tmp/taskai-helper-qa.LHAIHZ`, private TASKAI_HOME `state`, outer tmux
`taskai-helper-qa-LHAIHZ`, runtime server `taskai-5e9b7c0a81c60605`.
Provider fixtures returned fixed version/auth-status responses; the launched
fake Codex was a raw stdin-recording process, labeled `ISOLATED FAKE HELPER —
NO MODEL CALLS`. SSH was a failing fixture. No real provider authentication,
credential reads, model calls, installation, user state, or user tmux tab changes.

## Passed

- F1 from Home opened the provider chooser. Before Enter, JSON still contained
  zero tasks/sessions and empty helper settings. The chooser showed its purpose,
  local provider choice, native model-access caveat, and no-automatic-prompt note.
- Choosing fake Codex created exactly ordinary `t1` / `s1`: local profile,
  Manager role with no parent, HOME/helper workspace, normal visible task/team
  counts. Generated AGENTS.md and README.md had the versioned helper guide;
  ordinary context bootstrap was also present.
- After launch stabilized, tmux `s1` PID was **43273**. Its raw stdin log was
  exactly **zero bytes**. No prompt text, Enter, or initial input was sent.
- `taskai helper guide` and `taskai helper status` worked without modifying JSON.
  Status explicitly described recorded state rather than claiming a fresh
  runtime/login check. This test does not treat fixture sign-in as real login.
- A request from `TASKAI_SESSION_REF=unregistered` was denied. A request from the
  exact helper `s1` to `add-server` was queued, not applied. The CLI explained
  Ctrl+G → F8 review, and the focused helper displayed a pending-request notice.
- Ctrl+G → F8 opened the ordinary Add Server form. JSON SHA-256 remained
  `257b792c073458e39cf2ed791c74194f876e9b2cfec44eb64fd71f96c71493f4`
  across read-only commands, rejected/accepted requests, opening the form,
  cancellation, and request dismissal. No server or additional agent was created.
- Typed `Keep this draft` in Add Server, then queued `new-task`. F8 while the
  settings form was active did not replace the form or its draft. Esc canceled
  the form; another Esc returned to task navigation where the pending request
  appeared. Esc dismissed it without navigating or changing registry data.
- F1 in task navigation reused `t1`, `s1`, and PID 43273. Typed only
  `QA_DIRECT_INPUT`, without Enter: raw log contained exactly those **15 bytes**.
  Ctrl+F toggled full-screen; Ctrl+G returned to navigation. Neither leaked into
  stdin. Settings → `? Help me set up` also reused the same task/Manager/PID and
  did not add input or records.
- At 110×30, Settings rendered Provider / Status / Version, focused provider
  details and model-access caveat, and all shortcuts across two footer lines.
  At 150×42, the focused helper occupied the normal main CLI area.
- Quit TaskAI from navigation. The private helper process remained PID 43273 with
  its 15-byte input log. Relaunch → F1 reattached to that same PID and previous
  output, with no additional input or helper records.
- Normal confirmed task destruction (`D`, exact `t1`) removed only the fixture
  helper task/Manager and killed its runtime. Saved helper refs cleared while
  preferred Codex remained. Guide/workspace files were preserved. F1 reopened the
  empty chooser without creating anything; Esc canceled it.

## Captures

- [Inline helper, 150×42](2026-09-06-helper-inline.txt)
- [Settings, 110×30](2026-09-06-helper-settings-110.txt)
- [Helper chooser, 110×30 after normal destroy](2026-09-06-helper-chooser-110.txt)

These are literal tmux cell captures, not simulated screenshots or a claim of
pixel-level terminal-emulator parity.

## Cleanup and limits

TaskAI quit normally. Both exact private tmux servers returned `no server
running`; no QA runtime remains. Fixture files remain for audit. This proof
covers reviewable helper navigation and persistence, not a billed provider turn,
real account/model access, or a real remote-server setup.
