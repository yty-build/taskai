// ABOUTME: Shared test guard for provider CLIs — a proof that needs an absent harness is
// ABOUTME: reported as unproven and skipped, never silently passed and never a false failure.

/// True when the provider CLI answers `--version` on this machine.
///
/// When it does not, the caller skips so a contributor without every harness still gets a
/// green suite. Setting `TASKAI_REQUIRE_HARNESSES` turns an absent harness into a failure
/// instead, which is how the release gate refuses to pass on unproven ground.
pub fn present(bin: &str) -> bool {
    let found = std::process::Command::new(bin)
        .arg("--version")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false);
    if !found {
        let notice = format!("unproven: {bin} is not installed, so this proof did not run");
        assert!(
            std::env::var("TASKAI_REQUIRE_HARNESSES").is_err(),
            "{notice} (TASKAI_REQUIRE_HARNESSES is set)"
        );
        eprintln!("{notice}");
    }
    found
}
