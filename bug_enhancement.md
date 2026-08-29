# Bug and enhancement: manually add a Session to a Manager

**Status:** Product-owner change request
**Date:** 2026-08-29
**Affected surface:** Task workspace, Session creation, Manager task awareness

## Summary

TaskAI currently lets the user add only a top-level Manager Session. A subordinate Session can be created only when the Manager model invokes the delegation capability. This prevents the user from explicitly adding a Claude Code, Codex, Grok, or other subordinate-capable provider Session beneath an existing Manager.

The Task workspace must allow the user to select a Manager and manually add a subordinate Session owned by that Manager. Manager-driven delegation remains available; manual creation is an additional supported creation path.

## Current behavior

In the Task workspace:

- `a` opens **Add Manager**.
- The creation form offers only enabled providers with a conformed Manager capability.
- There is no visible or keyboard-accessible action for adding a subordinate Session beneath the selected Manager.
- The user must submit a natural-language prompt and hope the Manager chooses to create the intended subordinate.

This behavior follows the current wording in `docs/UX_PRODUCT_VISION.md`, but that wording does not match the intended product behavior.

## Contract conflict to correct

The authoritative product contracts currently state that:

- Add Session creates a Manager Session.
- Subordinate Sessions are created by a Manager Session.
- Only the Manager model invokes the typed delegation capability.

The product contracts must be amended before implementation so they also authorize a user-initiated, metadata-only subordinate creation path. Creating a subordinate from the TaskAI interface is a lifecycle operation; it does not prompt the subordinate, interpret content, or invoke delegation on behalf of the Manager model.

## Required behavior

### Add a subordinate Session

From the Task workspace:

1. The user selects a Manager Session.
2. The user invokes **Add Session**.
3. TaskAI shows all enabled and connected providers with a conformed Subordinate capability.
4. The user chooses a provider and may provide an optional Session label.
5. TaskAI creates the provider-native Session and records only the permitted TaskAI Session metadata and opaque provider Session reference.
6. The new Session is attached to the selected Manager and appears indented beneath it.
7. The selected Manager owns all subsequent prompt, Answer, Follow, Replay, Resume, and cancellation operations for that subordinate.
8. Creating the Session sends no synthetic prompt and creates no TaskAI transcript content.

The user does not directly prompt the subordinate from the TaskAI interface. The Manager decides when and how to use the attached Session.

### Preserve Manager creation

The Task workspace must expose two distinct actions:

- **Add Session**: create a subordinate beneath the selected Manager.
- **Add Manager**: create another top-level Manager in the Task.

Recommended keyboard bindings:

- `a`: Add Session to the selected Manager.
- `m`: Add Manager.

Both actions must be visible in the footer and Help. If no Manager is selected, Add Session is unavailable with a direct explanation.

### Manager awareness

On the Manager's next interaction, its current Task topology must identify the newly attached subordinate, including its TaskAI Session ID, provider, label, ownership edge, lifecycle state, and supported mechanical operations. No prompt, answer, summary, tool traffic, or replay content may be included in this topology.

If the Session is added while the Manager is already processing a turn, the Manager must receive the updated topology at the next supported model-interaction boundary. Older topology generations are historical; the highest generation is authoritative.

## Ownership and lifecycle rules

- A manually created subordinate has exactly one selected parent Manager in the same Task.
- A subordinate remains one level deep in V1.
- A manually created subordinate is indistinguishable in ownership and later behavior from one created through Manager delegation.
- The Manager's typed capability may target the attached subordinate by its TaskAI Session ID.
- TaskAI must not infer a prompt, assignment, or semantic purpose from the label or creation action.
- Provider authentication remains available only through Settings; Add Session must never launch a surprise login flow.
- Failure to create the provider-native Session must not leave orphaned TaskAI metadata.
- Viewer detach must not stop the Manager or its attached subordinate Sessions.

## UX clarification

The Add Manager form must make its state explicit:

- Title: **Create a top-level Manager**.
- Primary action: **Create Manager**.
- Explanation: this creates a Manager, not a subordinate Session.
- After creation, the workspace must visibly indicate which Manager is selected and show `Tab` or `i` as the action for entering its prompt input.

The Task workspace should use **open Manager input** or **focus Manager input**, rather than the ambiguous phrase **enter Manager**, when describing the navigation action.

## Acceptance criteria

1. With Manager A selected, `a` opens Add Session and lists enabled subordinate-capable providers.
2. Creating a Claude subordinate places it beneath Manager A and not beneath Manager B.
3. `m` still opens Add Manager and creates a top-level Manager.
4. The manually attached subordinate accepts work only from Manager A.
5. The Manager can target the attached Session through the same typed connector capability used for Manager-created subordinates.
6. The next Manager interaction receives a current topology generation containing the attached Session.
7. TaskAI persistence contains only permitted Task and Session metadata, settings, and the opaque native Session reference.
8. No prompt, answer, synthetic message, replay item, tool content, or transcript is created by the Add Session action.
9. Exact Resume continues to accept only the stored opaque provider Session reference.
10. Unsupported, disabled, disconnected, or unauthenticated providers fail closed with a direct Settings path.
11. Help and footer key hints accurately distinguish Add Session from Add Manager at every supported terminal size.
12. PTY acceptance coverage proves creation, hierarchy placement, Manager ownership, detach/reattach, exact Resume, and clean failure without orphaned metadata.

## Required contract updates before implementation

Update these authoritative documents in order:

1. `docs/UX_PRODUCT_VISION.md`
2. `build_plan.md`
3. `docs/TEST_PLAN.md`

The updates must replace the Manager-only meaning of Add Session, authorize manual subordinate creation without authorizing direct subordinate prompting, add distinct Add Session and Add Manager interactions, and add executable conformance and acceptance coverage for the new lifecycle path.
