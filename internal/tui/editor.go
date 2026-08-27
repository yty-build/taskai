package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// editor is an intentionally small RAM-only single-line editor. It retains the
// original UTF-8 bytes for submission and never persists or logs a draft.
type editor struct {
	value  string
	cursor int // byte offset on a UTF-8 boundary
	limit  int
}

func newEditor(limit int) editor { return editor{limit: limit} }

func (e *editor) insert(text string) {
	if text == "" || e.limit <= 0 {
		return
	}
	var clean strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\r' || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			continue
		}
		clean.WriteRune(r)
	}
	insert := clean.String()
	remaining := e.limit - len(e.value)
	if remaining <= 0 {
		return
	}
	if len(insert) > remaining {
		insert = validPrefix(insert, remaining)
	}
	e.value = e.value[:e.cursor] + insert + e.value[e.cursor:]
	e.cursor += len(insert)
}

func (e *editor) left() {
	if e.cursor <= 0 {
		return
	}
	_, size := utf8.DecodeLastRuneInString(e.value[:e.cursor])
	e.cursor -= size
}

func (e *editor) right() {
	if e.cursor >= len(e.value) {
		return
	}
	_, size := utf8.DecodeRuneInString(e.value[e.cursor:])
	e.cursor += size
}

func (e *editor) backspace() {
	if e.cursor <= 0 {
		return
	}
	_, size := utf8.DecodeLastRuneInString(e.value[:e.cursor])
	start := e.cursor - size
	e.value = e.value[:start] + e.value[e.cursor:]
	e.cursor = start
}

func (e *editor) delete() {
	if e.cursor >= len(e.value) {
		return
	}
	_, size := utf8.DecodeRuneInString(e.value[e.cursor:])
	e.value = e.value[:e.cursor] + e.value[e.cursor+size:]
}

func (e *editor) home() { e.cursor = 0 }
func (e *editor) end()  { e.cursor = len(e.value) }

func (e *editor) clear() {
	e.value = ""
	e.cursor = 0
}

func validPrefix(value string, bytes int) string {
	if len(value) <= bytes {
		return value
	}
	value = value[:bytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
