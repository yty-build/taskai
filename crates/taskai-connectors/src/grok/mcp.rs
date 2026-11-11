// ABOUTME: Grok workspace MCP install plus the stateless stdio MCP client that
// ABOUTME: relays list_sessions and send_message to the session's tool socket.

use std::io::{self, BufRead, Write};
use std::path::Path;

use serde_json::{json, Value};

use super::tools::{atomic_write, call_tool};

use toml_edit::{value, Array, DocumentMut, InlineTable, Item, Table, Value as TomlValue};

use crate::Error;

const TASKAI_SERVER: &str = "taskai";
const ALLOW_RULE: &str = "MCPTool(taskai__*)";

/// Install `[mcp_servers.taskai]` and the matching allow rule into the
/// workspace project file. Existing user tables stay; a re-install updates
/// TaskAI's command/args in place and strips stored socket overrides.
pub fn install_mcp(workspace: &Path, taskai_bin: &Path) -> Result<(), Error> {
    let dir = workspace.join(".grok");
    std::fs::create_dir_all(&dir)?;
    let path = dir.join("config.toml");
    let mut doc = if path.exists() {
        std::fs::read_to_string(&path)?
            .parse::<DocumentMut>()
            .map_err(|e| Error::Invalid(format!("{}: {e}", path.display())))?
    } else {
        DocumentMut::new()
    };

    merge_taskai(&mut doc, taskai_bin)?;
    upsert_allow(&mut doc)?;
    atomic_write(&path, &doc.to_string())?;
    Ok(())
}

fn merge_taskai(doc: &mut DocumentMut, taskai_bin: &Path) -> Result<(), Error> {
    let servers = table_at(&mut doc["mcp_servers"], "mcp_servers")?;
    match servers.get_mut(TASKAI_SERVER) {
        None => {
            let mut taskai = Table::new();
            taskai.set_implicit(false);
            apply_taskai_table(&mut taskai, taskai_bin)?;
            servers[TASKAI_SERVER] = Item::Table(taskai);
        }
        Some(Item::Table(taskai)) => {
            apply_taskai_table(taskai, taskai_bin)?;
        }
        Some(Item::Value(TomlValue::InlineTable(inline))) => {
            apply_taskai_inline(inline, taskai_bin)?;
        }
        Some(_) => {
            return Err(Error::Invalid(
                "workspace .grok/config.toml [mcp_servers.taskai] is not a table".into(),
            ));
        }
    }
    Ok(())
}

fn apply_taskai_table(taskai: &mut Table, taskai_bin: &Path) -> Result<(), Error> {
    taskai["command"] = value(taskai_bin.display().to_string());
    taskai["args"] = value(Array::from_iter(["mcp"]));
    strip_stored_socket(&mut taskai["env"])
}

fn apply_taskai_inline(taskai: &mut InlineTable, taskai_bin: &Path) -> Result<(), Error> {
    taskai.insert("command", taskai_bin.display().to_string().into());
    taskai.insert("args", Array::from_iter(["mcp"]).into());
    match taskai.get_mut("env") {
        None => return Ok(()),
        Some(TomlValue::InlineTable(env)) => {
            env.remove("TASKAI_TOOL_SOCKET");
            if !env.is_empty() {
                return Ok(());
            }
        }
        Some(_) => {
            return Err(Error::Invalid(
                "workspace .grok/config.toml [mcp_servers.taskai] env is not a table".into(),
            ));
        }
    }
    taskai.remove("env");
    Ok(())
}

fn strip_stored_socket(item: &mut Item) -> Result<(), Error> {
    match item {
        Item::None => Ok(()),
        Item::Value(TomlValue::InlineTable(env)) => {
            env.remove("TASKAI_TOOL_SOCKET");
            if env.is_empty() {
                *item = Item::None;
            }
            Ok(())
        }
        Item::Table(env) => {
            env.remove("TASKAI_TOOL_SOCKET");
            if env.is_empty() {
                *item = Item::None;
            }
            Ok(())
        }
        _ => Err(Error::Invalid(
            "workspace .grok/config.toml [mcp_servers.taskai] env is not a table".into(),
        )),
    }
}

fn table_at<'a>(item: &'a mut Item, name: &str) -> Result<&'a mut Table, Error> {
    match item {
        Item::None => {
            let mut table = Table::new();
            table.set_implicit(true);
            *item = Item::Table(table);
        }
        Item::Value(TomlValue::InlineTable(inline)) => {
            let mut table = Table::new();
            table.set_implicit(true);
            for (key, val) in inline.clone() {
                table.insert(&key, Item::Value(val));
            }
            *item = Item::Table(table);
        }
        Item::Table(_) => {}
        _ => {
            return Err(Error::Invalid(format!(
                "workspace .grok/config.toml [{name}] is not a table"
            )));
        }
    }
    Ok(item.as_table_mut().expect("table"))
}

fn upsert_allow(doc: &mut DocumentMut) -> Result<(), Error> {
    if doc.get("permission").is_none() {
        let mut permission = Table::new();
        permission["allow"] = value(Array::from_iter([ALLOW_RULE]));
        doc.insert("permission", Item::Table(permission));
        return Ok(());
    }
    let permission = table_at(&mut doc["permission"], "permission")?;
    match permission.get_mut("allow") {
        None => {
            permission["allow"] = value(Array::from_iter([ALLOW_RULE]));
        }
        Some(item) => {
            let Some(arr) = item.as_array_mut() else {
                return Err(Error::Invalid(
                    "workspace .grok/config.toml [permission] allow is not an array".into(),
                ));
            };
            let already = arr.iter().any(|v| v.as_str() == Some(ALLOW_RULE));
            if !already {
                arr.push(ALLOW_RULE);
            }
        }
    }
    Ok(())
}

const TOOLS: &str = r#"[
  {
    "name": "list_sessions",
    "description": "List the live sessions you can message: name, harness, status and what a waiting session waits for.",
    "inputSchema": {"type": "object", "properties": {}}
  },
  {
    "name": "send_message",
    "description": "Send a text message to another session, addressed by its session name. Set notify_when_idle to be told once when that session next goes idle or exits.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "to": {"type": "string", "description": "Recipient session name."},
        "text": {"type": "string", "description": "Message text."},
        "notify_when_idle": {"type": "boolean", "description": "Also subscribe to one idle notice."}
      },
      "required": ["to", "text"]
    }
  }
]"#;

/// One MCP JSON-RPC message. Notifications (no id) produce no reply. The
/// process holds no mailbox address; GROK_SESSION_ID is the only identity.
pub fn handle_mcp_message(msg: &Value, tool_socket: &Path, session_id: &str) -> Option<Value> {
    let id = msg.get("id")?;
    let method = msg["method"].as_str().unwrap_or("");
    let params = msg.get("params").cloned().unwrap_or_else(|| json!({}));
    let reply = match method {
        "initialize" => json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {
                "protocolVersion": params.get("protocolVersion").cloned().unwrap_or_else(|| json!("2025-06-18")),
                "capabilities": {"tools": {"listChanged": false}},
                "serverInfo": {"name": "taskai", "version": "0.2.0"}
            }
        }),
        "tools/list" => json!({
            "jsonrpc": "2.0",
            "id": id,
            "result": {"tools": serde_json::from_str::<Value>(TOOLS).expect("tools")}
        }),
        "tools/call"
            if matches!(
                params["name"].as_str(),
                Some("list_sessions") | Some("send_message")
            ) =>
        {
            let args = params
                .get("arguments")
                .cloned()
                .unwrap_or_else(|| json!({}));
            let result = call_tool(
                tool_socket,
                session_id,
                params["name"].as_str().unwrap_or(""),
                args,
            );
            let ok = matches!(result["status"].as_str(), Some("ok") | Some("sent"));
            json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": {
                    "content": [{"type": "text", "text": result.to_string()}],
                    "isError": !ok
                }
            })
        }
        "ping" => json!({"jsonrpc": "2.0", "id": id, "result": {}}),
        _ => json!({
            "jsonrpc": "2.0",
            "id": id,
            "error": {"code": -32601, "message": format!("method not found: {method}")}
        }),
    };
    Some(reply)
}

/// Stdio MCP server for `taskai mcp`. Identity comes from GROK_SESSION_ID.
pub fn run_mcp_stdio() -> io::Result<()> {
    let socket = std::env::var("TASKAI_TOOL_SOCKET").unwrap_or_default();
    let session_id = std::env::var("GROK_SESSION_ID").unwrap_or_default();
    let stdin = io::stdin();
    let mut stdout = io::stdout();
    for line in stdin.lock().lines() {
        let line = line?;
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Ok(msg) = serde_json::from_str::<Value>(line) else {
            continue;
        };
        if let Some(reply) = handle_mcp_message(&msg, Path::new(&socket), &session_id) {
            writeln!(stdout, "{reply}")?;
            stdout.flush()?;
        }
    }
    Ok(())
}
