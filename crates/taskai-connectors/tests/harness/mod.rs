// ABOUTME: Shared test guard for provider CLIs — a proof whose harness is absent or below
// ABOUTME: the supported version is reported unproven and skipped, never failed or faked.

use taskai_connectors::{Capability, Mode};

/// True when the harness is installed *and* within its connector's supported version
/// range, which is the only state in which its proofs mean anything.
///
/// Otherwise the caller skips, so a contributor missing a CLI — or carrying an older one
/// on their PATH — still gets a green suite instead of a misleading failure. Setting
/// `TASKAI_REQUIRE_HARNESSES` turns that skip into a failure, which is how the release
/// gate refuses to pass on unproven ground.
pub fn ready(name: &str, cap: &Capability) -> bool {
    if cap.mode != Mode::Unsupported {
        return true;
    }
    let notice = format!(
        "unproven: {name} is absent or unsupported ({}) — this proof did not run",
        cap.detail
    );
    assert!(
        std::env::var("TASKAI_REQUIRE_HARNESSES").is_err(),
        "{notice} (TASKAI_REQUIRE_HARNESSES is set)"
    );
    eprintln!("{notice}");
    false
}
