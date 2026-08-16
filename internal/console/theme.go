package console

import "strings"

// consoleTone names meaning rather than a provider or fixed color. The ANSI
// palette is intentionally restrained and remains readable in 256-color
// terminals; render width is computed before these zero-width codes are added.
type consoleTone uint8

const (
	tonePlain consoleTone = iota
	toneBrand
	toneSection
	toneSelected
	toneUser
	toneAgent
	toneMuted
	toneSuccess
	toneWarning
	toneDanger
	toneComposer
	toneFooter
)

type consoleTheme struct {
	styles map[consoleTone]string
}

var cockpitTheme = consoleTheme{styles: map[consoleTone]string{
	toneBrand:    "1;38;5;159",
	toneSection:  "1;38;5;252",
	toneSelected: "1;38;5;159",
	toneUser:     "1;38;5;222",
	toneAgent:    "1;38;5;117",
	toneMuted:    "38;5;245",
	toneSuccess:  "38;5;114",
	toneWarning:  "1;38;5;214",
	toneDanger:   "1;38;5;203",
	toneComposer: "1;38;5;252",
	toneFooter:   "38;5;250;48;5;236",
}}

func (t consoleTheme) paint(tone consoleTone, line string) string {
	style := t.styles[tone]
	if style == "" || strings.TrimSpace(line) == "" {
		return line
	}
	return "\x1b[" + style + "m" + line + "\x1b[0m"
}

func decorateLine(line string, footer bool) string {
	tone := tonePlain
	switch {
	case strings.HasPrefix(line, "◆ "):
		tone = toneBrand
	case strings.HasPrefix(line, "  SESSIONS"), strings.HasPrefix(line, "  SELECTED SESSION"), strings.HasPrefix(line, "  LIVE ACP"),
		strings.HasPrefix(line, "  RECENT CONVERSATION"), strings.HasPrefix(line, "  CONTEXT"),
		strings.HasPrefix(line, "  CONTINUE"), strings.HasPrefix(line, "  PROVENANCE"), strings.HasPrefix(line, "  SESSION"):
		tone = toneSection
	case strings.HasPrefix(line, "▌ "), strings.HasPrefix(line, "│ S-"):
		if left, right, split := strings.Cut(line, " │ "); split {
			return cockpitTheme.paint(toneSelected, left) + " " + cockpitTheme.paint(toneMuted, "│") + " " + right
		}
		tone = toneSelected
	case strings.HasPrefix(line, "❯ YOU"):
		tone = toneUser
	case strings.HasPrefix(line, "● "):
		tone = toneAgent
	case strings.HasPrefix(line, "  ▸ Reasoning"), strings.HasPrefix(line, "  ▾ Reasoning"), strings.HasPrefix(line, "  ◇ "):
		tone = toneMuted
	case strings.HasPrefix(line, "  ├─ ✓ TOOL"), strings.HasPrefix(line, "  ✓ Turn complete"), strings.HasPrefix(line, "✓ RESPONSE"):
		tone = toneSuccess
	case strings.HasPrefix(line, "  ╭─ APPROVAL REQUIRED"), strings.HasPrefix(line, "CONFIRM ·"):
		tone = toneWarning
	case strings.HasPrefix(line, "! ERROR"), strings.HasPrefix(line, "  ! "):
		tone = toneDanger
	case strings.HasPrefix(line, "╭─ MESSAGE"):
		tone = toneComposer
	case strings.HasPrefix(line, "╰─ "), strings.HasPrefix(line, "│  "), strings.Trim(line, "─ │") == "":
		tone = toneMuted
	case footer:
		tone = toneFooter
	}
	return cockpitTheme.paint(tone, line)
}
