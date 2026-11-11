# Retired: doctor's global Claude card scan

`live_cards.rs` is the `taskai doctor` fixture that enumerated every `<pid>.json` card of a live
Claude process and fed them to `drift_check`, with its test. Retired on 2026-11-06 by the lead's
M7 review: the common readiness check for Claude is version and envelope support only, and exact
TaskAI-owned native cards are validated by the M6 observer and the M3 mirror. A foreign process's
card is not TaskAI's readiness evidence, and the scan read arbitrary native state without a bound.
