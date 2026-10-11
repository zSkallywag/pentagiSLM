package smallmodel

// RecoveryDirective is injected at the top of the prompt when the agent is
// detected looping on a failing approach. A small model tends to vary the
// parameters of a broken approach (different timeouts, wrappers) instead of
// abandoning it; this tells it to switch mechanism rather than retry.
const RecoveryDirective = "RECOVERY ALERT: your recent attempts keep failing in the same way (see FAILED ATTEMPTS below). Do NOT keep varying the same approach — that will not work. Switch to a fundamentally different mechanism, tool, or strategy; if nothing works, report the blocker plainly instead of retrying."

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
