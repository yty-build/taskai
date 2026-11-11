// ABOUTME: Claude Code's cross-session-message envelope: canonical rendering and strict parsing, ported
// ABOUTME: from the release drift::TESTED_VERSION names: attribute order, value rules, name rule, escape.

use std::path::Path;

use taskai_core::mail::PermissionMode;

/// The envelope's tag.
pub const TAG: &str = "cross-session-message";
/// Longest name or plugin Claude renders before cutting it with an ellipsis, in characters.
const MAX_NAME_CHARS: usize = 64;
/// Tokens a hop chain may hold, each this many lowercase hex digits.
const MAX_CHAIN_TOKENS: usize = 32;
const HOP_TOKEN_HEX: usize = 24;
/// Longest `from-session` value.
const MAX_SESSION_CHARS: usize = 80;

/// One envelope: who sent the body, as Claude states it, and the body itself.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Envelope {
    /// The sender's reply address, `uds:<socket path>`.
    pub from: Option<String>,
    pub from_session: Option<String>,
    pub hop_chain: Option<Vec<String>>,
    pub from_name: Option<String>,
    pub from_mode: Option<PermissionMode>,
    pub from_plugin: Option<String>,
    pub body: String,
}

impl Envelope {
    /// The envelope as Claude renders it: attributes in Claude's order, each only when present and
    /// valid, names by Claude's name rule, and the body escaped as Claude escapes it.
    pub fn render(&self) -> String {
        let mut attributes = Vec::new();
        if let Some(from) = self.from.as_deref().filter(|from| !from.is_empty()) {
            attributes.push(format!("from=\"{from}\""));
        }
        if let Some(session) = self.from_session.as_deref().filter(|s| is_session(s)) {
            attributes.push(format!("from-session=\"{session}\""));
        }
        if let Some(chain) = self.hop_chain.as_ref().filter(|chain| !chain.is_empty()) {
            let joined = chain.join(",");
            if is_chain(&joined) {
                attributes.push(format!("hop-chain=\"{joined}\""));
            }
        }
        if let Some(name) = self.from_name.as_deref().map(shown_name) {
            if !name.is_empty() {
                attributes.push(format!("from-name=\"{name}\""));
            }
        }
        if let Some(mode) = self.from_mode {
            attributes.push(format!("from-mode=\"{}\"", mode_name(mode)));
        }
        if let Some(plugin) = self.from_plugin.as_deref().map(shown_name) {
            if !plugin.is_empty() {
                attributes.push(format!("from-plugin=\"{plugin}\""));
            }
        }
        let head = if attributes.is_empty() {
            String::new()
        } else {
            format!(" {}", attributes.join(" "))
        };
        format!(
            "<{TAG}{head}>\n{}\n</{TAG}>",
            escape_closing_tags(&self.body)
        )
    }

    /// The envelope `text` is, if it is one exactly as Claude would render it; anything else,
    /// including a reordered, unknown or invalid attribute, is not an envelope.
    pub fn parse(text: &str) -> Option<Envelope> {
        let rest = text.strip_prefix('<')?.strip_prefix(TAG)?;
        let (from, rest) = attribute(rest, "from", is_from);
        let (from_session, rest) = attribute(rest, "from-session", is_session);
        let (hop_chain, rest) = attribute(rest, "hop-chain", is_chain);
        let (from_name, rest) = attribute(rest, "from-name", is_shown_text);
        let (from_mode, rest) = attribute(rest, "from-mode", |mode| parse_mode(mode).is_some());
        let (from_plugin, rest) = attribute(rest, "from-plugin", is_shown_text);
        let body = rest
            .strip_prefix(">\n")?
            .strip_suffix(&format!("\n</{TAG}>"))?;
        let envelope = Envelope {
            from,
            from_session,
            hop_chain: hop_chain.map(|chain| chain.split(',').map(str::to_string).collect()),
            from_name,
            from_mode: from_mode.as_deref().and_then(parse_mode),
            from_plugin,
            body: body.into(),
        };
        (envelope.render() == text).then_some(envelope)
    }
}

/// Claude's `from` address for a socket: `uds:` and the path, with every byte outside letters,
/// digits and `:_/.-` percent-encoded.
pub fn uds_address(socket: &Path) -> String {
    let mut address = String::from("uds:");
    for byte in socket.as_os_str().as_encoded_bytes() {
        if byte.is_ascii_alphanumeric() || b":_/.-".contains(byte) {
            address.push(char::from(*byte));
        } else {
            address.push_str(&format!("%{byte:02X}"));
        }
    }
    address
}

/// Reads ` name="value"` when it comes next and `value` is valid; otherwise leaves `rest` as is.
fn attribute<'a>(
    rest: &'a str,
    name: &str,
    valid: impl Fn(&str) -> bool,
) -> (Option<String>, &'a str) {
    let Some(value_and_rest) = rest
        .strip_prefix(' ')
        .and_then(|rest| rest.strip_prefix(name))
        .and_then(|rest| rest.strip_prefix("=\""))
    else {
        return (None, rest);
    };
    match value_and_rest.split_once('"') {
        Some((value, after)) if valid(value) => (Some(value.into()), after),
        _ => (None, rest),
    }
}

fn mode_name(mode: PermissionMode) -> &'static str {
    match mode {
        PermissionMode::Prompting => "prompting",
        PermissionMode::Bypass => "bypass",
    }
}

fn parse_mode(mode: &str) -> Option<PermissionMode> {
    match mode {
        "prompting" => Some(PermissionMode::Prompting),
        "bypass" => Some(PermissionMode::Bypass),
        _ => None,
    }
}

fn is_from(value: &str) -> bool {
    !value.is_empty()
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"%:_/.-".contains(&b))
}

fn is_session(value: &str) -> bool {
    (1..=MAX_SESSION_CHARS).contains(&value.len())
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn is_chain(value: &str) -> bool {
    let tokens: Vec<&str> = value.split(',').collect();
    tokens.len() <= MAX_CHAIN_TOKENS
        && tokens.iter().all(|token| {
            token.len() == HOP_TOKEN_HEX
                && token
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        })
}

/// A name or plugin value as the parser reads it: no quote, angle bracket or line break.
fn is_shown_text(value: &str) -> bool {
    !value.is_empty() && !value.contains(['"', '<', '>', '\n', '\r'])
}

/// Claude's name rule: quotes and angle brackets dropped, control, format and line or paragraph
/// separator characters removed, surrounding space trimmed, and more than 64 characters cut to
/// 64 and an ellipsis.
fn shown_name(name: &str) -> String {
    let kept: String = name
        .chars()
        .filter(|c| !matches!(c, '"' | '<' | '>') && !is_unshown(*c))
        .collect();
    let trimmed = kept.trim();
    if trimmed.chars().count() > MAX_NAME_CHARS {
        let cut: String = trimmed.chars().take(MAX_NAME_CHARS).collect();
        format!("{cut}\u{2026}")
    } else {
        trimmed.into()
    }
}

/// Unicode control (Cc), format (Cf), line separator (Zl) and paragraph separator (Zp)
/// characters; surrogates (Cs) cannot occur in a `str`.
fn is_unshown(c: char) -> bool {
    matches!(c,
        '\u{0}'..='\u{1F}' | '\u{7F}'..='\u{9F}' | '\u{AD}' | '\u{600}'..='\u{605}' | '\u{61C}'
        | '\u{6DD}' | '\u{70F}' | '\u{890}'..='\u{891}' | '\u{8E2}' | '\u{180E}'
        | '\u{200B}'..='\u{200F}' | '\u{2028}'..='\u{202E}' | '\u{2060}'..='\u{2064}'
        | '\u{2066}'..='\u{206F}' | '\u{FEFF}' | '\u{FFF9}'..='\u{FFFB}' | '\u{110BD}'
        | '\u{110CD}' | '\u{13430}'..='\u{1343F}' | '\u{1BCA0}'..='\u{1BCA3}'
        | '\u{1D173}'..='\u{1D17A}' | '\u{E0001}' | '\u{E0020}'..='\u{E007F}')
}

/// Claude's body escape: a backslash after the `<` (an ASCII one, or a lookalike, which becomes
/// ASCII) of anything that reads as a closing envelope tag, so a body cannot end its envelope.
fn escape_closing_tags(body: &str) -> String {
    let mut escaped = String::with_capacity(body.len());
    for (at, c) in body.char_indices() {
        let rest = &body[at + c.len_utf8()..];
        if is_open(c) && !rest.starts_with('\\') && closes_envelope(rest) {
            escaped.push_str("<\\");
        } else {
            escaped.push(c);
        }
    }
    escaped
}

/// True when what follows a `<` reads as `/cross-session-message` by Claude's pattern: filler that
/// is not a slash, a slash, filler, then the tag's letters in any case, invisible characters
/// allowed between them and any dash or underscore lookalike for each dash, then no word
/// character. Each run is taken whole, as Claude's possessive groups take it.
fn closes_envelope(rest: &str) -> bool {
    let mut chars = rest.chars().peekable();
    while chars.next_if(|&c| is_filler(c) && !is_slash(c)).is_some() {}
    if chars.next_if(|&c| is_slash(c)).is_none() {
        return false;
    }
    while chars.next_if(|&c| is_filler(c)).is_some() {}
    for (index, wanted) in TAG.chars().enumerate() {
        if index > 0 {
            while chars.next_if(|&c| is_invisible(c)).is_some() {}
        }
        let matched = match chars.next() {
            Some(c) if wanted == '-' => is_dash_like(c),
            Some(c) => folds_to(c, wanted),
            None => false,
        };
        if !matched {
            return false;
        }
    }
    chars.peek().is_none_or(|&c| !is_word(c))
}

/// Matches a tag letter case-insensitively as Claude's Unicode pattern does: the long s folds to s.
fn folds_to(c: char, letter: char) -> bool {
    c.to_ascii_lowercase() == letter || (letter == 's' && c == '\u{17F}')
}

/// Letters, digits, `_` and `-`, with the long s and the Kelvin sign, which fold to letters.
fn is_word(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '\u{17F}' | '\u{212A}')
}

fn is_filler(c: char) -> bool {
    !is_word(c) && !is_open(c) && !is_close(c)
}

/// `<` and every character Claude's lookalike table reads as one.
fn is_open(c: char) -> bool {
    matches!(
        c,
        '<' | '\u{2C2}'
            | '\u{1438}'
            | '\u{16B2}'
            | '\u{2039}'
            | '\u{226A}'
            | '\u{226E}'
            | '\u{227A}'
            | '\u{2280}'
            | '\u{22D6}'
            | '\u{22D8}'
            | '\u{2329}'
            | '\u{276C}'
            | '\u{276E}'
            | '\u{2770}'
            | '\u{27E8}'
            | '\u{27EA}'
            | '\u{2991}'
            | '\u{2993}'
            | '\u{29FC}'
            | '\u{2A79}'
            | '\u{2AA1}'
            | '\u{2AA6}'
            | '\u{2AF7}'
            | '\u{3008}'
            | '\u{FE3D}'
            | '\u{FE3F}'
            | '\u{FE64}'
            | '\u{FF1C}'
            | '\u{1D236}'
    )
}

/// `>` and every character Claude's lookalike table reads as one. Claude leaves the doubled
/// closing brackets and a few others out of that table, so they are ordinary filler here too.
fn is_close(c: char) -> bool {
    matches!(
        c,
        '>' | '\u{2C3}'
            | '\u{1433}'
            | '\u{203A}'
            | '\u{226F}'
            | '\u{227B}'
            | '\u{2281}'
            | '\u{22D7}'
            | '\u{232A}'
            | '\u{276D}'
            | '\u{276F}'
            | '\u{2771}'
            | '\u{27E9}'
            | '\u{29FD}'
            | '\u{3009}'
            | '\u{FE40}'
            | '\u{FE65}'
            | '\u{FF1E}'
            | '\u{16F3F}'
            | '\u{1D237}'
    )
}

fn is_slash(c: char) -> bool {
    matches!(c, '/' | '\u{FF0F}' | '\u{2215}' | '\u{2044}')
}

/// Characters Claude lets sit invisibly between a tag's letters: format and filler characters,
/// combining marks, and controls other than tab, line feed and carriage return.
fn is_invisible(c: char) -> bool {
    matches!(c,
        '\u{0}'..='\u{8}' | '\u{B}' | '\u{C}' | '\u{E}'..='\u{1F}' | '\u{7F}'..='\u{9F}'
        | '\u{2028}' | '\u{2029}' | '\u{AD}' | '\u{34F}' | '\u{600}'..='\u{605}' | '\u{61C}'
        | '\u{6DD}' | '\u{70F}' | '\u{890}' | '\u{891}' | '\u{8E2}' | '\u{115F}' | '\u{1160}'
        | '\u{17B4}' | '\u{17B5}' | '\u{180B}'..='\u{180F}' | '\u{200B}'..='\u{200F}'
        | '\u{202A}'..='\u{202E}' | '\u{2060}'..='\u{206F}' | '\u{3164}' | '\u{FE00}'..='\u{FE0F}'
        | '\u{FEFF}' | '\u{FFA0}' | '\u{FFF0}'..='\u{FFFB}' | '\u{110BD}' | '\u{110CD}'
        | '\u{13430}'..='\u{1343F}' | '\u{1BCA0}'..='\u{1BCA3}' | '\u{1D173}'..='\u{1D17A}'
        | '\u{16FE4}' | '\u{E0000}'..='\u{E0FFF}'
        | '\u{300}'..='\u{344}' | '\u{346}'..='\u{36F}' | '\u{483}'..='\u{489}'
        | '\u{591}'..='\u{5BD}' | '\u{5BF}' | '\u{5C1}' | '\u{5C2}' | '\u{5C4}' | '\u{5C5}'
        | '\u{5C7}' | '\u{610}'..='\u{61A}' | '\u{64B}'..='\u{65F}' | '\u{670}'
        | '\u{6D6}'..='\u{6DC}' | '\u{6DF}'..='\u{6E4}' | '\u{6E7}' | '\u{6E8}'
        | '\u{6EA}'..='\u{6ED}' | '\u{1AB0}'..='\u{1AFF}' | '\u{1DC0}'..='\u{1DFF}'
        | '\u{20D0}'..='\u{20FF}' | '\u{3099}' | '\u{309A}' | '\u{FE20}'..='\u{FE2F}')
}

/// What may stand for a dash in the tag: underscores and connector punctuation (Pc), dash
/// punctuation (Pd), and the minus and dash lookalikes Claude lists.
fn is_dash_like(c: char) -> bool {
    matches!(c,
        '_' | '\u{203F}' | '\u{2040}' | '\u{2054}' | '\u{FE33}' | '\u{FE34}'
        | '\u{FE4D}'..='\u{FE4F}' | '\u{FF3F}'
        | '-' | '\u{58A}' | '\u{5BE}' | '\u{1400}' | '\u{1806}' | '\u{2010}'..='\u{2015}'
        | '\u{2E17}' | '\u{2E1A}' | '\u{2E3A}' | '\u{2E3B}' | '\u{2E40}' | '\u{2E5D}'
        | '\u{301C}' | '\u{3030}' | '\u{30A0}' | '\u{FE31}' | '\u{FE32}' | '\u{FE58}'
        | '\u{FE63}' | '\u{FF0D}' | '\u{10D6E}' | '\u{10EAD}'
        | '\u{2017}' | '\u{2CD}' | '\u{7FA}' | '\u{640}' | '\u{2212}' | '\u{207B}' | '\u{208B}'
        | '\u{2D7}' | '\u{2796}' | '\u{2043}' | '\u{30FC}' | '\u{FF70}')
}
