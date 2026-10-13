package smallmodel

import (
	"fmt"
	"regexp"
	"strings"
)

// A small model reaches for a wordlist credential attack as a first move — even
// when the task already hands it a credential, or before any service
// enumeration — and then grinds it for hours. The strategy gate makes an
// expensive wordlist brute-force a last resort: it intercepts the command before
// execution and replays the facts the agent already holds, so it uses a known
// credential or finishes enumeration first. It targets only the expensive
// wordlist attack (a user/pass list), never a single targeted login, and the
// caller fires it at most once per task, then trusts the agent's judgment.

var (
	// bruteForceTool names the credential-guessing tools. These exist to try
	// many candidates; on their own they do not imply a wordlist run.
	bruteForceTool = regexp.MustCompile(`(?i)\b(hydra|medusa|ncrack|patator)\b`)
	// listFlag marks the expensive form via a short list flag. It is
	// CASE-SENSITIVE on purpose: hydra/medusa take -P/-L/-U/-C for lists and
	// -p/-l/-u for single values, so a case-insensitive match would gate a
	// single targeted guess.
	listFlag = regexp.MustCompile(`(?:^|\s)-[PLUC](?:\s|=)`)
	// listLong catches the long flags and the standard wordlist indicators,
	// where case does not disambiguate a list from a single value.
	listLong = regexp.MustCompile(`(?i)(?:--passwords?|--userlist|--combo|--wordlist)\b|rockyou|/usr/share/wordlists`)
)

// IsWordlistBruteForce reports whether cmd launches a wordlist-driven credential
// attack (a brute-force tool plus a list/wordlist argument).
func IsWordlistBruteForce(cmd string) bool {
	return bruteForceTool.MatchString(cmd) && (listFlag.MatchString(cmd) || listLong.MatchString(cmd))
}

// knownCredentialFacts returns the credential facts already in state.
func knownCredentialFacts(facts []string) []string {
	var out []string
	for _, f := range facts {
		if strings.HasPrefix(f, CredentialFactPrefix) {
			out = append(out, f)
		}
	}
	return out
}

// hasReconFacts reports whether any service/port has been enumerated yet.
func hasReconFacts(facts []string) bool {
	for _, f := range facts {
		if strings.Contains(f, "/tcp open") || strings.Contains(f, "/udp open") || strings.Contains(f, "open:") {
			return true
		}
	}
	return false
}

// StrategyGate decides whether a proposed command is a premature wordlist
// brute-force. It gates when the agent already holds a credential (it should
// authenticate with it) or has not enumerated the target yet (it should look for
// provided, default or discoverable credentials first). A brute-force after recon
// with no credential in hand is a legitimate last resort and passes. The returned
// message replays the current facts and the untried alternatives.
func StrategyGate(cmd string, facts []string) (gated bool, message string) {
	if !IsWordlistBruteForce(cmd) {
		return false, ""
	}
	if creds := knownCredentialFacts(facts); len(creds) > 0 {
		return true, strategyMessage(
			"you already hold a credential (see ESTABLISHED FACTS) — authenticate with it before a wordlist attack",
			facts,
		)
	}
	if !hasReconFacts(facts) {
		return true, strategyMessage(
			"no service enumeration is recorded yet — enumerate the target and look for provided, default or discoverable credentials first",
			facts,
		)
	}
	return false, ""
}

func strategyMessage(reason string, facts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "STRATEGY GATE: a wordlist brute-force was held back because %s. A wordlist attack is a last resort, not a first move.\n", reason)
	b.WriteString("Facts you already have:\n")
	if len(facts) == 0 {
		b.WriteString("  (none yet)\n")
	} else {
		for _, f := range facts {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	b.WriteString("Before brute-forcing, state which of these you have NOT yet tried: " +
		"a credential given in the task, default or well-known credentials, credentials found in recon " +
		"(config files, web pages, repositories, service banners), SSH key authentication, anonymous or guest " +
		"access, or a known CVE for the detected service version. Brute-force only once these are exhausted.")
	return strings.TrimRight(b.String(), "\n")
}
