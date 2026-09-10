// ABOUTME: Single-line form editing and cell-aware input rendering. Native CLI input
// ABOUTME: never passes through this editor.
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use taskai_tui::color::{bg, fg, DIM, GLASS, GLASS_IN, ION, RESET, TEXT};
use unicode_width::UnicodeWidthChar;

#[derive(Default, Clone, Copy)]
pub struct Editor {
    pub cursor: Option<usize>, // UTF-8 byte boundary; None means end
    pub selected: bool,
}

impl Editor {
    pub fn position(&self, value: &str) -> usize {
        self.cursor
            .filter(|&p| p <= value.len() && value.is_char_boundary(p))
            .unwrap_or(value.len())
    }

    pub fn insert(&mut self, value: &mut String, text: &str) {
        if self.selected {
            value.clear();
            self.cursor = Some(0);
        }
        let p = self.position(value);
        value.insert_str(p, text);
        self.cursor = Some(p + text.len());
        self.selected = false;
    }

    pub fn key(&mut self, value: &mut String, key: KeyEvent) -> bool {
        let p = self.position(value);
        let prev = value[..p].char_indices().next_back().map_or(0, |(i, _)| i);
        let next = value[p..].chars().next().map_or(p, |c| p + c.len_utf8());
        if key.modifiers.difference(KeyModifiers::SHIFT) == KeyModifiers::CONTROL {
            match key.code {
                KeyCode::Char('a' | 'A') => self.selected = true,
                KeyCode::Char('u' | 'U') => {
                    value.clear();
                    *self = Self::default();
                }
                KeyCode::Char('k' | 'K') => {
                    value.truncate(p);
                    self.selected = false;
                }
                KeyCode::Char('w' | 'W') | KeyCode::Backspace => {
                    self.delete_previous_word(value);
                }
                KeyCode::Left => {
                    self.cursor = Some(if self.selected {
                        0
                    } else {
                        previous_word(value, p)
                    });
                    self.selected = false;
                }
                KeyCode::Right => {
                    self.cursor = Some(if self.selected {
                        value.len()
                    } else {
                        next_word(value, p)
                    });
                    self.selected = false;
                }
                _ => return false,
            }
            return true;
        }
        if key.modifiers == KeyModifiers::ALT {
            match key.code {
                KeyCode::Left | KeyCode::Char('b') => {
                    self.cursor = Some(if self.selected {
                        0
                    } else {
                        previous_word(value, p)
                    });
                    self.selected = false;
                }
                KeyCode::Right | KeyCode::Char('f') => {
                    self.cursor = Some(if self.selected {
                        value.len()
                    } else {
                        next_word(value, p)
                    });
                    self.selected = false;
                }
                KeyCode::Backspace => self.delete_previous_word(value),
                _ => return false,
            }
            return true;
        }
        if key.modifiers.intersects(
            KeyModifiers::CONTROL
                | KeyModifiers::ALT
                | KeyModifiers::SUPER
                | KeyModifiers::META
                | KeyModifiers::HYPER,
        ) {
            return false;
        }
        match key.code {
            KeyCode::Left => self.cursor = Some(if self.selected { 0 } else { prev }),
            KeyCode::Right => self.cursor = Some(if self.selected { value.len() } else { next }),
            KeyCode::Home => self.cursor = Some(0),
            KeyCode::End => self.cursor = Some(value.len()),
            KeyCode::Backspace | KeyCode::Delete if self.selected => {
                value.clear();
                self.cursor = Some(0);
            }
            KeyCode::Backspace => {
                value.replace_range(prev..p, "");
                self.cursor = Some(prev);
            }
            KeyCode::Delete => {
                value.replace_range(p..next, "");
                self.cursor = Some(p);
            }
            KeyCode::Char(c) if !c.is_control() => {
                self.insert(value, &c.to_string());
                return true;
            }
            _ => return false,
        }
        self.selected = false;
        true
    }

    fn delete_previous_word(&mut self, value: &mut String) {
        let end = self.position(value);
        if self.selected {
            value.clear();
            self.cursor = Some(0);
        } else {
            let start = previous_word(value, end);
            value.replace_range(start..end, "");
            self.cursor = Some(start);
        }
        self.selected = false;
    }
}

fn previous_word(value: &str, cursor: usize) -> usize {
    let mut start = cursor;
    let mut crossed_word = false;
    for (index, ch) in value[..cursor].char_indices().rev() {
        if ch.is_whitespace() && crossed_word {
            break;
        }
        crossed_word |= !ch.is_whitespace();
        start = index;
    }
    start
}

fn next_word(value: &str, cursor: usize) -> usize {
    let mut end = cursor;
    let mut crossed_word = false;
    for (index, ch) in value[cursor..].char_indices() {
        if ch.is_whitespace() && crossed_word {
            break;
        }
        crossed_word |= !ch.is_whitespace();
        end = cursor + index + ch.len_utf8();
    }
    end
}

/// Strip pasted terminal padding while preserving legitimate internal path spaces.
/// Wrapped path lines are joined, not converted into dozens of embedded spaces.
pub fn clean_paste(text: &str, path: bool) -> String {
    // tmux paste-buffer and some terminals use bare CR between copied rows.
    text.replace("\r\n", "\n")
        .replace('\r', "\n")
        .replace('\t', " ")
        .lines()
        .map(|line| {
            line.trim()
                .chars()
                .filter(|c| !c.is_control())
                .collect::<String>()
        })
        .filter(|line| !line.is_empty())
        .collect::<Vec<_>>()
        .join(if path { "" } else { " " })
}

pub fn cell_width(c: char) -> usize {
    c.width().unwrap_or(0)
}

pub fn window_start(value: &str, editor: Editor, width: usize) -> usize {
    let p = editor.position(value);
    let mut used = 0;
    let mut start = p;
    for (i, c) in value[..p].char_indices().rev() {
        let w = cell_width(c);
        if used + w >= width.saturating_sub(1) {
            break;
        }
        used += w;
        start = i;
    }
    start
}

pub fn clicked_position(value: &str, editor: Editor, width: usize, col: usize) -> usize {
    let start = window_start(value, editor, width);
    let mut used = usize::from(start > 0);
    for (i, c) in value[start..].char_indices() {
        if used + cell_width(c) > col {
            return start + i;
        }
        used += cell_width(c);
    }
    value.len()
}

/// Render a full input row, reserving space for a clickable clear action. The
/// caret stays visible for long paths; selected text uses an explicit highlight.
pub fn input_row(value: &str, editor: Editor, focused: bool, width: usize) -> String {
    let room = width.saturating_sub(16);
    let edit = if focused {
        editor
    } else {
        Editor {
            cursor: Some(0),
            selected: false,
        }
    };
    let start = window_start(value, edit, room);
    let p = edit.position(value);
    let mut out = format!("{}  {} ", bg(GLASS), bg(GLASS_IN));
    let mut used = 0;
    if start > 0 {
        out.push_str(&fg(DIM));
        out.push('‹');
        used += 1;
    }
    let clipped_right = value[start..].chars().map(cell_width).sum::<usize>() + used > room;
    let text_room = room.saturating_sub(usize::from(clipped_right));
    for (i, c) in value[start..].char_indices() {
        let w = cell_width(c);
        if used + w > text_room {
            break;
        }
        let caret = focused && start + i == p;
        let marked = focused && (editor.selected || caret);
        out.push_str(&bg(if marked { ION } else { GLASS_IN }));
        out.push_str(&fg(if marked {
            GLASS_IN
        } else if focused {
            TEXT
        } else {
            DIM
        }));
        out.push(c);
        used += w;
    }
    if focused && p == value.len() && used < text_room {
        out.push_str(&bg(ION));
        out.push(' ');
        used += 1;
    }
    out.push_str(&bg(GLASS_IN));
    out.push_str(&" ".repeat(text_room.saturating_sub(used)));
    if clipped_right {
        out.push_str(&fg(DIM));
        out.push('›');
    }
    out.push_str(&fg(ION));
    out.push_str("  [Clear] ");
    out.push_str(&bg(GLASS));
    out.push_str("   ");
    out.push_str(RESET);
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn paste_trims_terminal_padding_but_preserves_spaces_in_paths() {
        assert_eq!(
            clean_paste(" /home/ubuntu/github/   \r smb-deal-flow ", true),
            "/home/ubuntu/github/smb-deal-flow"
        );
        assert_eq!(
            clean_paste("  /home/ubuntu/github/   \r\n smb-deal-flow  \n", true),
            "/home/ubuntu/github/smb-deal-flow"
        );
        assert_eq!(clean_paste(" /home/my project ", true), "/home/my project");
        assert_eq!(clean_paste(" ssh cruise2 \r\n", false), "ssh cruise2");
    }
    #[test]
    fn selection_replaces_and_unicode_editing_is_safe() {
        let mut value = "old repository".to_string();
        let mut e = Editor::default();
        e.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('a'), KeyModifiers::CONTROL),
        );
        e.insert(&mut value, "répo界");
        e.key(&mut value, KeyEvent::new(KeyCode::Left, KeyModifiers::NONE));
        e.key(
            &mut value,
            KeyEvent::new(KeyCode::Backspace, KeyModifiers::NONE),
        );
        assert_eq!(value, "rép界");
        e.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('u'), KeyModifiers::CONTROL),
        );
        assert!(value.is_empty());
    }
    #[test]
    fn long_path_end_and_clear_are_visible_at_exact_cell_width() {
        let value = format!("/{}final-repo", "long/".repeat(60));
        let row = input_row(&value, Editor::default(), true, 98);
        let plain = taskai_tui::seg::strip_sgr(&row);
        assert!(plain.contains("final-repo"));
        assert!(plain.contains("[Clear]"));
        assert_eq!(plain.chars().map(cell_width).sum::<usize>(), 98);
    }

    #[test]
    fn conventional_word_editing_works_without_leaking_shortcuts_into_text() {
        let mut value = "ssh ubuntu@host -p 2222".to_string();
        let mut editor = Editor::default();
        assert!(editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('w'), KeyModifiers::CONTROL)
        ));
        assert_eq!(value, "ssh ubuntu@host -p ");
        assert!(editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Left, KeyModifiers::CONTROL)
        ));
        assert_eq!(editor.position(&value), "ssh ubuntu@host ".len());
        assert!(editor.key(&mut value, KeyEvent::new(KeyCode::Left, KeyModifiers::ALT)));
        assert_eq!(editor.position(&value), "ssh ".len());
        assert!(editor.key(&mut value, KeyEvent::new(KeyCode::Right, KeyModifiers::ALT)));
        assert_eq!(editor.position(&value), "ssh ubuntu@host".len());
        assert!(editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Backspace, KeyModifiers::ALT)
        ));
        assert_eq!(value, "ssh  -p ");
        assert!(!editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL)
        ));
    }

    #[test]
    fn word_deletion_respects_unicode_and_select_all() {
        let mut value = "東京 café 界".to_string();
        let mut editor = Editor::default();
        editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Backspace, KeyModifiers::CONTROL),
        );
        assert_eq!(value, "東京 café ");
        editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('w'), KeyModifiers::CONTROL),
        );
        assert_eq!(value, "東京 ");
        editor.key(
            &mut value,
            KeyEvent::new(
                KeyCode::Char('A'),
                KeyModifiers::CONTROL | KeyModifiers::SHIFT,
            ),
        );
        editor.key(
            &mut value,
            KeyEvent::new(KeyCode::Char('w'), KeyModifiers::CONTROL),
        );
        assert_eq!(value, "");
        assert_eq!(editor.position(&value), 0);
    }

    #[test]
    fn clipped_fields_show_both_directions_without_changing_cell_width() {
        let value = format!("/{}final-repo", "東京/".repeat(60));
        for width in [30, 98, 138] {
            for cursor in [0, value.len() / 2, value.len()] {
                let cursor = (0..=cursor)
                    .rev()
                    .find(|&p| value.is_char_boundary(p))
                    .unwrap();
                let row = input_row(
                    &value,
                    Editor {
                        cursor: Some(cursor),
                        selected: false,
                    },
                    true,
                    width,
                );
                let plain = taskai_tui::seg::strip_sgr(&row);
                assert_eq!(plain.chars().map(cell_width).sum::<usize>(), width);
                assert!(plain.contains("[Clear]"));
                if cursor == 0 {
                    assert!(plain.contains('›'));
                }
                if cursor == value.len() {
                    assert!(plain.contains('‹'));
                    assert!(plain.contains("final-repo"));
                }
            }
        }
    }
}
