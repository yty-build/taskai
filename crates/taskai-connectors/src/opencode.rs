// ABOUTME: OpenCode connector — shared-live: one authenticated loopback `opencode serve`
// ABOUTME: shared by the native TUI (attach) and the TaskAI HTTP facade.

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use crate::claude::parse_version;
use crate::{Capability, Error, Mode};

pub struct OpenCodeConnector {
    pub bin: String,
}

impl Default for OpenCodeConnector {
    fn default() -> Self {
        OpenCodeConnector {
            bin: "opencode".into(),
        }
    }
}

impl OpenCodeConnector {
    /// Harmless handshake: reads the version, never creates a session.
    pub fn probe(&self) -> Capability {
        match Command::new(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                crate::gate_version(
                    Capability {
                        mode: Mode::SharedLive,
                        version: parse_version(&text),
                        detail: text,
                    },
                    "1.18.0",
                )
            }
            Ok(out) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: String::from_utf8_lossy(&out.stderr).trim().to_string(),
            },
            Err(e) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: e.to_string(),
            },
        }
    }

    /// Starts the authenticated loopback server in the workspace and waits for readiness.
    pub fn start_server(&self, dir: &Path) -> Result<Server, Error> {
        let port = free_port()?;
        let password = format!("taskai-{}", random_hex());
        let child = Command::new(&self.bin)
            .args([
                "serve",
                "--port",
                &port.to_string(),
                "--hostname",
                "127.0.0.1",
            ])
            .current_dir(dir)
            .env("OPENCODE_SERVER_PASSWORD", &password)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()?;
        let server = Server {
            child,
            host: format!("127.0.0.1:{port}"),
            password,
            dir: dir.to_path_buf(),
            bin: self.bin.clone(),
        };
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            if let Ok((200, _)) = server.http("GET", "/session", None) {
                return Ok(server);
            }
            if Instant::now() >= deadline {
                return Err(Error::Timeout("opencode serve never became ready".into()));
            }
            std::thread::sleep(Duration::from_millis(250));
        }
    }
}

pub struct Server {
    child: Child,
    host: String,
    pub password: String,
    pub dir: PathBuf,
    bin: String,
}

impl Server {
    pub fn url(&self) -> String {
        format!("http://{}", self.host)
    }

    fn raw_http(
        &self,
        method: &str,
        path: &str,
        body: Option<&str>,
        auth: bool,
    ) -> Result<(u16, String), Error> {
        let mut stream = TcpStream::connect(&self.host)?;
        stream.set_read_timeout(Some(Duration::from_secs(15)))?;
        let body = body.unwrap_or("");
        let mut req = format!(
            "{method} {path} HTTP/1.1\r\nHost: {}\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n",
            self.host,
            body.len()
        );
        if auth {
            req.push_str(&format!(
                "Authorization: Basic {}\r\n",
                b64(format!("opencode:{}", self.password).as_bytes())
            ));
        }
        req.push_str("\r\n");
        req.push_str(body);
        stream.write_all(req.as_bytes())?;
        let mut resp = Vec::new();
        stream.read_to_end(&mut resp)?;
        let resp = String::from_utf8_lossy(&resp).to_string();
        let code = resp
            .split_whitespace()
            .nth(1)
            .and_then(|c| c.parse().ok())
            .unwrap_or(0);
        let payload = resp
            .split_once("\r\n\r\n")
            .map(|(_, b)| b.to_string())
            .unwrap_or_default();
        Ok((code, payload))
    }

    pub fn http(
        &self,
        method: &str,
        path: &str,
        body: Option<&str>,
    ) -> Result<(u16, String), Error> {
        self.raw_http(method, path, body, true)
    }

    pub fn http_unauthenticated(
        &self,
        method: &str,
        path: &str,
        body: Option<&str>,
    ) -> Result<(u16, String), Error> {
        self.raw_http(method, path, body, false)
    }

    /// Creates a provider session bound to the server's workspace directory.
    pub fn create_session(&self) -> Result<String, Error> {
        let (code, body) = self.http("POST", "/session", Some("{}"))?;
        if code != 200 {
            return Err(Error::Invalid(format!(
                "create session: HTTP {code}: {body}"
            )));
        }
        extract_after(&body, "\"id\":\"")
            .ok_or_else(|| Error::Invalid(format!("no session id in {body}")))
    }

    /// Subscribes to the server's SSE event stream; events arrive as parsed JSON.
    pub fn subscribe(&self) -> Result<std::sync::mpsc::Receiver<serde_json::Value>, Error> {
        let mut stream = TcpStream::connect(&self.host)?;
        let req = format!(
            "GET /event HTTP/1.1\r\nHost: {}\r\nAccept: text/event-stream\r\nAuthorization: Basic {}\r\n\r\n",
            self.host,
            b64(format!("opencode:{}", self.password).as_bytes())
        );
        stream.write_all(req.as_bytes())?;
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            let reader = std::io::BufReader::new(stream);
            for line in std::io::BufRead::lines(reader).map_while(Result::ok) {
                if let Some(data) = line.strip_prefix("data: ") {
                    if let Ok(v) = serde_json::from_str::<serde_json::Value>(data) {
                        if tx.send(v).is_err() {
                            break;
                        }
                    }
                }
            }
        });
        Ok(rx)
    }

    /// The exact native TUI the pane spawns, attached to this same server.
    pub fn attach_command(&self) -> (String, Vec<String>, Vec<(String, String)>) {
        (
            self.bin.clone(),
            vec!["attach".into(), self.url()],
            vec![("OPENCODE_SERVER_PASSWORD".into(), self.password.clone())],
        )
    }
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}

fn extract_after(haystack: &str, prefix: &str) -> Option<String> {
    let start = haystack.find(prefix)? + prefix.len();
    let end = haystack[start..].find('"')? + start;
    Some(haystack[start..end].to_string())
}

fn free_port() -> Result<u16, Error> {
    let listener = TcpListener::bind("127.0.0.1:0")?;
    Ok(listener.local_addr()?.port())
}

fn random_hex() -> String {
    let mut buf = [0u8; 8];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut buf);
    }
    buf.iter().map(|b| format!("{b:02x}")).collect()
}

fn b64(input: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for chunk in input.chunks(3) {
        let b = [
            chunk[0],
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        ];
        let n = ((b[0] as u32) << 16) | ((b[1] as u32) << 8) | b[2] as u32;
        out.push(T[(n >> 18) as usize & 63] as char);
        out.push(T[(n >> 12) as usize & 63] as char);
        out.push(if chunk.len() > 1 {
            T[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            T[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}
