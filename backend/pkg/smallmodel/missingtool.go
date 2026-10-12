package smallmodel

import (
	"fmt"
	"regexp"
	"strings"
)

// A small model reads a "command not found" line, does not connect it to the
// missing package, and keeps re-running the absent tool — the exact loop a
// 13B model fell into on sshpass. The signal is present in the output but gets
// lost in a long, noisy chain. DetectMissingTools lifts it out so it can be
// recorded as a durable fact at the top of the prompt, where the model acts on
// it: given the clean signal it provisions the tool itself the large majority
// of the time, so this surfaces the observation rather than installing anything.
var (
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// "<cmd>: command not found" / "<cmd>: not found", with optional shell and
	// line-number prefixes (bash, sh/dash "sh: 1:", "bash: line 1:"). The command
	// is captured whole so a token carrying a colon fails the validity check
	// below instead of being silently cropped to a plausible-looking name.
	notFoundSuffix = regexp.MustCompile(`(?i)(?:^|\n)\s*(?:(?:bash|sh|zsh|dash|ash|ksh|/bin/[a-z]+)\s*:\s*)?(?:line\s+\d+\s*:\s*|\d+\s*:\s*)*(.+?)\s*:\s*(?:command\s+)?not found\b`)
	// zsh's inverted form: "zsh: command not found: <cmd>".
	notFoundPrefix = regexp.MustCompile(`(?i)command not found:\s*(\S+)`)
	// A real tool name: starts alphanumeric, then word/dot/plus/dash chars only.
	// This rejects "student:password" (a mistyped command the shell also reports
	// as "not found") and multi-word fragments.
	toolName = regexp.MustCompile(`^[A-Za-z0-9][\w.+-]*$`)
)

// alwaysPresent are shells that exist in any sandbox, so they are never a
// missing tool to install. The suffix rule also captures the leading shell name
// of the inverted form "zsh: command not found: foo"; excluding them here drops
// that spurious hit and leaves the real tool to the prefix rule.
var alwaysPresent = map[string]struct{}{
	"bash": {}, "sh": {}, "zsh": {}, "dash": {}, "ash": {}, "ksh": {},
}

// DetectMissingTools returns the names of tools the shell reported as absent in
// output, de-duplicated in first-seen order. It strips ANSI colour codes first
// (sandbox output carries them) and keeps only tokens that look like a command
// name, so a mistyped command reported as "not found" does not become a bogus
// install target.
func DetectMissingTools(output string) []string {
	if output == "" {
		return nil
	}
	clean := ansiEscape.ReplaceAllString(output, "")

	seen := make(map[string]struct{})
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || !toolName.MatchString(name) {
			return
		}
		if _, shell := alwaysPresent[strings.ToLower(name)]; shell {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	for _, m := range notFoundSuffix.FindAllStringSubmatch(clean, -1) {
		add(m[1])
	}
	for _, m := range notFoundPrefix.FindAllStringSubmatch(clean, -1) {
		add(m[1])
	}
	return out
}

// MissingToolFact phrases a missing tool as an actionable, durable fact for the
// state block. The phrasing stays true after the tool is installed ("if you
// still need it"), so a stale copy never contradicts a later success.
func MissingToolFact(name string) string {
	return fmt.Sprintf(
		"MISSING TOOL: %q was not found in the sandbox when last run — if you still need it, install it first (e.g. 'apt-get update && apt-get install -y %s'), then use it.",
		name, name,
	)
}
