package console

import (
	"strings"
	"unicode"

	"github.com/yty-build/acp-gateway/internal/session"
)

type nativeCLICapability struct {
	available bool
	label     string
	status    string
	reason    string
}

func nativeCapability(current session.Session) nativeCLICapability {
	agent := strings.ToLower(strings.TrimSpace(current.Agent))
	label, supported := nativeCLILabel(agent)
	if !supported {
		return nativeCLICapability{
			label:  label,
			status: "unavailable",
			reason: "this agent has no supported exact native resume command; use Structured ACP",
		}
	}
	if strings.TrimSpace(current.ProviderConversationID) == "" {
		return nativeCLICapability{
			label:  label,
			status: "unavailable",
			reason: "waiting for the exact provider conversation identity; use Structured ACP now",
		}
	}
	if strings.TrimSpace(current.WorktreePath) == "" && strings.TrimSpace(current.RepoPath) == "" {
		return nativeCLICapability{
			label:  label,
			status: "unavailable",
			reason: "the Session has no checkout path for the provider CLI; use Structured ACP",
		}
	}
	status := "identity present · safety checked on open"
	if current.ConversationIDVerifiedAt != nil {
		status = "identity verified · worker checked on open"
	}
	return nativeCLICapability{available: true, label: label, status: status}
}

func nativeCLILabel(agent string) (string, bool) {
	switch agent {
	case "codex":
		return "Codex CLI", true
	case "claude":
		return "Claude CLI", true
	case "grok", "grok-build":
		return "Grok CLI", true
	case "opencode":
		return "OpenCode CLI", true
	case "gemini":
		return "Gemini CLI", false
	case "":
		return "Native CLI", false
	default:
		return providerDisplayName(agent) + " CLI", false
	}
}

func providerDisplayName(agent string) string {
	if agent == "" {
		return "Provider"
	}
	runes := []rune(strings.ReplaceAll(agent, "-", " "))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
