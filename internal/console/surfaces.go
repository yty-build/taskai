package console

import (
	"strings"
	"unicode"
)

func providerDisplayName(agent string) string {
	if agent == "" {
		return "Provider"
	}
	runes := []rune(strings.ReplaceAll(agent, "-", " "))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
