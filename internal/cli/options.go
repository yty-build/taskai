package cli

import (
	"fmt"
	"strings"
)

type optionKind int

const (
	stringOption optionKind = iota
	boolOption
)

type parsedOptions struct {
	values      map[string]string
	booleans    map[string]bool
	positionals []string
}

func parseOptions(args []string, allowed map[string]optionKind) (parsedOptions, error) {
	result := parsedOptions{values: make(map[string]string), booleans: make(map[string]bool)}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			result.positionals = append(result.positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			result.positionals = append(result.positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		kind, ok := allowed[name]
		if !ok {
			return parsedOptions{}, fmt.Errorf("unknown option %q", name)
		}
		if seen[name] {
			return parsedOptions{}, fmt.Errorf("option %q was provided more than once", name)
		}
		seen[name] = true
		switch kind {
		case boolOption:
			if hasValue {
				return parsedOptions{}, fmt.Errorf("option %q does not take a value", name)
			}
			result.booleans[name] = true
		case stringOption:
			if !hasValue {
				i++
				if i >= len(args) {
					return parsedOptions{}, fmt.Errorf("option %q requires a value", name)
				}
				value = args[i]
			}
			result.values[name] = value
		}
	}
	return result, nil
}
