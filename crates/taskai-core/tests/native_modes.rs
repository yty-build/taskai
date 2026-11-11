// ABOUTME: Native address-book mode metadata can be absent without inventing a permission claim.
// ABOUTME: Every transmitted letter and subscription still requires its sender's declared mode.

use serde_json::{json, Value};
use taskai_core::{mail::Frame, registry::MessagingEntry};

fn metadata() -> Value {
    json!({
        "pid": 123,
        "procStart": "native-start",
        "mailbox": "/private/native.sock",
        "features": ["notify_idle"],
        "status": "idle"
    })
}

#[test]
fn native_metadata_can_omit_mode_without_fabricating_it() {
    let entry: MessagingEntry =
        serde_json::from_value(metadata()).expect("a native card need not report permission mode");
    let saved = serde_json::to_value(entry).unwrap();
    assert!(saved.get("mode").is_none(), "unknown mode must stay absent");
}

#[test]
fn known_mode_metadata_remains_backward_compatible() {
    for mode in ["prompting", "bypass"] {
        let mut value = metadata();
        value["mode"] = json!(mode);
        let entry: MessagingEntry = serde_json::from_value(value.clone()).unwrap();
        assert_eq!(serde_json::to_value(entry).unwrap(), value);
    }
}

#[test]
fn letters_and_subscriptions_still_require_real_mode() {
    for kind in ["message", "subscribe"] {
        let mut value = json!({"type": kind, "msg_id": "one", "from": "claude-lead"});
        if kind == "message" {
            value["body"] = json!("letter");
        }
        assert!(serde_json::from_value::<Frame>(value.clone()).is_err());
        for invalid in [Value::Null, json!("unknown")] {
            value["from_mode"] = invalid;
            assert!(serde_json::from_value::<Frame>(value.clone()).is_err());
        }
        for mode in ["prompting", "bypass"] {
            value["from_mode"] = json!(mode);
            let frame: Frame = serde_json::from_value(value.clone()).unwrap();
            assert_eq!(serde_json::to_value(frame).unwrap()["from_mode"], mode);
        }
    }
}
