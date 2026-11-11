// ABOUTME: Isolated process proof that install_mcp writes only the workspace
// ABOUTME: .grok/config.toml and never HOME or GROK_HOME global Grok config.

use std::path::PathBuf;

use taskai_connectors::grok::install_mcp;

fn tmp() -> tempfile::TempDir {
    tempfile::Builder::new()
        .prefix("t5")
        .tempdir_in("/tmp")
        .unwrap()
}

// Expectation: install writes only the workspace file, even when HOME and
// GROK_HOME point at empty temp dirs. Why: global provider config is
// off-limits, and this binary is a dedicated process so HOME injection
// cannot leak into parallel grok_mail tests. Value: a test or a launch
// cannot mutate the person's Grok home.
#[test]
fn mcp_install_never_writes_global_grok_home() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    let home = tmp.path().join("home");
    let grok_home = tmp.path().join("grok-home");
    std::fs::create_dir_all(&home).unwrap();
    std::fs::create_dir_all(&grok_home).unwrap();
    std::env::set_var("HOME", &home);
    std::env::set_var("GROK_HOME", &grok_home);
    install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap();
    let config = ws.join(".grok/config.toml");
    assert!(config.is_file());
    let body = std::fs::read_to_string(&config).unwrap();
    assert!(
        !body.contains("TASKAI_TOOL_SOCKET"),
        "socket must not be stored: {body}"
    );
    assert!(!home.join(".grok/config.toml").exists());
    assert!(!grok_home.join("config.toml").exists());
}
