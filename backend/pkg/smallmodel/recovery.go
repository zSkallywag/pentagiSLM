package smallmodel

import (
	"fmt"
	"regexp"
	"strings"
)

// RecoveryDirective is injected at the top of the prompt when the agent is
// detected looping on a failing approach. A small model tends to vary the
// parameters of a broken approach (different timeouts, wrappers) instead of
// abandoning it; this tells it to switch mechanism rather than retry.
const RecoveryDirective = "RECOVERY ALERT: your recent attempts keep failing in the same way (see FAILED ATTEMPTS below). Do NOT keep varying the same approach — that will not work. Switch to a fundamentally different mechanism, tool, or strategy; if nothing works, report the blocker plainly instead of retrying."

// RepetitionRecoveryDirective names the specific repeated action, so the model
// knows exactly which gesture to abandon. Used by the repetition-based detector.
func RepetitionRecoveryDirective(canonical string) string {
	return fmt.Sprintf(
		"RECOVERY ALERT: you have run the same action (%s) several times with no new findings — it is not working. "+
			"Do NOT run it again. Switch to a fundamentally different mechanism or tool, or report the blocker plainly.",
		canonical,
	)
}

var (
	reCanonANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// RE2 has no backreferences, so the closing delimiter cannot be matched; a
	// here-doc opener collapses everything from "<<DELIM" to the end of the
	// command, which is where a script body sits.
	reCanonHeredoc = regexp.MustCompile(`<<-?\s*["']?\w+["']?[\s\S]*$`)
	reCanonQuoted  = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	reCanonPath    = regexp.MustCompile(`(?:/[\w.\-]+)+`)
	reCanonNumber  = regexp.MustCompile(`\d+`)
	reCanonSpace   = regexp.MustCompile(`\s+`)
)

// CanonicalizeCommand reduces a shell command to its shape, so superficially
// different runs of the same gesture collapse to one fingerprint: here-doc
// bodies and quoted strings become placeholders (the body of a rewritten script
// no longer matters), a path keeps only its directory and extension
// (/work/a_final.py and /work/b_final.py both become /work/*.py), and every
// number becomes N (a varied timeout, port or IP no longer distinguishes). It is
// deliberately lossy: it answers "is this the same gesture?", not "what exactly
// did it do?".
func CanonicalizeCommand(cmd string) string {
	s := reCanonANSI.ReplaceAllString(cmd, "")
	s = reCanonHeredoc.ReplaceAllString(s, "<<HEREDOC")
	s = reCanonQuoted.ReplaceAllString(s, "<str>")
	s = reCanonPath.ReplaceAllStringFunc(s, canonicalizePath)
	s = reCanonNumber.ReplaceAllString(s, "N")
	return strings.TrimSpace(reCanonSpace.ReplaceAllString(s, " "))
}

// canonicalizePath keeps a path's directory and extension but drops the stem, so
// files that differ only by name in the same place share a fingerprint.
func canonicalizePath(p string) string {
	dir, base := p, ""
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		dir, base = p[:i], p[i+1:]
	}
	ext := ""
	if j := strings.LastIndexByte(base, '.'); j > 0 {
		ext = base[j:]
	}
	return dir + "/*" + ext
}

// RepeatedCanonical returns the canonical form that appears at least minRepeats
// times within the last window entries, which marks a repeated gesture. The
// caller passes already-canonicalized commands.
func RepeatedCanonical(recent []string, window, minRepeats int) (string, bool) {
	start := 0
	if len(recent) > window {
		start = len(recent) - window
	}
	counts := make(map[string]int)
	for _, c := range recent[start:] {
		if c == "" {
			continue
		}
		counts[c]++
		if counts[c] >= minRepeats {
			return c, true
		}
	}
	return "", false
}

// DetectStuckByRepetition reports the agent looping on one gesture while learning
// nothing new: a canonical command repeated minRepeats times in the last window,
// AND no new fact recorded for at least progressK commands. The no-progress gate
// is what separates a loop from a legitimate repeated action (a brute-force or a
// sweep that is still covering new ground keeps resetting cmdsSinceProgress).
func DetectStuckByRepetition(recent []string, cmdsSinceProgress, window, minRepeats, progressK int) (string, bool) {
	if cmdsSinceProgress < progressK {
		return "", false
	}
	return RepeatedCanonical(recent, window, minRepeats)
}

// DetectStuck reports whether the last `window` failed attempts are mutually
// similar, which signals the agent is repeating one failing approach rather
// than trying genuinely different ones. Similarity is token-set overlap, so it
// catches semantic repetition that exact-match loop detection misses.
func DetectStuck(failedTries []string, window int, threshold float64) bool {
	n := len(failedTries)
	if window < 2 || n < window {
		return false
	}
	recent := failedTries[n-window:]

	sets := make([]map[string]struct{}, len(recent))
	for i, r := range recent {
		sets[i] = toSet(tokenize(r))
	}

	var sum float64
	var pairs int
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			sum += jaccard(sets[i], sets[j])
			pairs++
		}
	}
	if pairs == 0 {
		return false
	}
	return sum/float64(pairs) >= threshold
}
