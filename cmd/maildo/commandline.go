package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandLine rebuilds the command the user ran, so maildo can print the
// exact command for the next step: the same search with --page 3, or a
// bulk preview with --apply. drop removes flags; set adds or replaces them
// ("" as the value means a bool flag that is switched on).
func commandLine(cmd *cobra.Command, args []string, drop []string, set [][2]string) string {
	parts := strings.Fields(cmd.CommandPath())
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	skip := map[string]bool{}
	for _, name := range drop {
		skip[name] = true
	}
	for _, pair := range set {
		skip[pair[0]] = true
	}
	// Visit in the order the user typed the flags.
	flags := cmd.Flags()
	sorted := flags.SortFlags
	flags.SortFlags = false
	defer func() { flags.SortFlags = sorted }()
	originalIDs, hasOriginal := cmd.Annotations[annotationShortIDs]
	flags.Visit(func(flag *pflag.Flag) {
		if skip[flag.Name] {
			return
		}
		// Show the short ids the user typed, not the full ids maildo put in
		// their place, and leave out the account maildo picked for them.
		if flag.Name == "account" && cmd.Annotations[annotationShortAccount] == "set" {
			return
		}
		if flag.Name == "id" && hasOriginal {
			for _, v := range strings.Split(originalIDs, "\x00") {
				parts = append(parts, "--id", shellQuote(v))
			}
			return
		}
		switch flag.Value.Type() {
		case "bool":
			if flag.Value.String() == "true" {
				parts = append(parts, "--"+flag.Name)
			} else {
				parts = append(parts, "--"+flag.Name+"=false")
			}
		case "stringArray", "stringSlice":
			if slice, ok := flag.Value.(pflag.SliceValue); ok {
				for _, v := range slice.GetSlice() {
					parts = append(parts, "--"+flag.Name, shellQuote(v))
				}
			}
		default:
			parts = append(parts, "--"+flag.Name, shellQuote(flag.Value.String()))
		}
	})
	for _, pair := range set {
		if pair[1] == "" {
			parts = append(parts, "--"+pair[0])
		} else {
			parts = append(parts, "--"+pair[0], shellQuote(pair[1]))
		}
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for a POSIX shell when it needs quoting.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@%+=,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
