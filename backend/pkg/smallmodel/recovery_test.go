package smallmodel

import (
	"strings"
	"testing"
)

func TestDetectStuck_FlagsRepeatedSimilarFailures(t *testing.T) {
	tries := []string{
		"terminal failed: ssh -o StrictHostKeyChecking=no student@192.168.1.0 timeout 20",
		"terminal failed: ssh -o StrictHostKeyChecking=no student@192.168.1.0 timeout 30",
		"terminal failed: ssh -o StrictHostKeyChecking=no student@192.168.1.0 timeout 45",
		"terminal failed: ssh -o StrictHostKeyChecking=no student@192.168.1.0 timeout 10",
	}
	if !DetectStuck(tries, 4, 0.5) {
		t.Fatal("expected repeated similar ssh failures to be detected as stuck")
	}
}

func TestDetectStuck_DoesNotFlagDiverseFailures(t *testing.T) {
	tries := []string{
		"terminal failed: nmap scan returned no open ports",
		"terminal failed: hydra could not find the password",
		"terminal failed: curl connection refused on port 8080",
		"terminal failed: sqlmap found no injectable parameter",
	}
	if DetectStuck(tries, 4, 0.5) {
		t.Fatal("genuinely different attempts should not be flagged as stuck")
	}
}

func TestDetectStuck_NeedsEnoughHistory(t *testing.T) {
	if DetectStuck([]string{"a", "a"}, 4, 0.5) {
		t.Fatal("should not flag with fewer entries than the window")
	}
}

func TestCanonicalizeCommand_CollapsesSuperficialVariants(t *testing.T) {
	eq := func(a, b string) {
		t.Helper()
		if ca, cb := CanonicalizeCommand(a), CanonicalizeCommand(b); ca != cb {
			t.Fatalf("expected same canonical form:\n  %q -> %q\n  %q -> %q", a, ca, b, cb)
		}
	}
	// varied timeout/thread/target numbers (flow 12/13 hydra grind)
	eq("timeout 180 hydra -t 1 -s 22 192.168.1.0", "timeout 300 hydra -t 4 -s 22 192.168.1.0")
	// different script names in the same dir (flow 14 rewrite-and-run)
	eq("python3 /work/ssh_enumeration_final.py", "python3 /work/ssh_user_context_check_final.py")
	// quoted search term varies, same gesture (flow 13 log re-reading)
	eq(`grep -a "Success" /work/hydra_ssh_attack.log | head -30`, `grep -a "13429" /work/hydra_ssh_attack.log | head -30`)
}

func TestCanonicalizeCommand_CollapsesHeredocBody(t *testing.T) {
	a := "cat > /work/a_final.py << 'EOF'\nimport paramiko\nprint('one')\nEOF"
	b := "cat > /work/b_check.py << 'EOF'\nimport socket\ndo_other_thing()\nEOF"
	if CanonicalizeCommand(a) != CanonicalizeCommand(b) {
		t.Fatalf("heredoc bodies should not distinguish the gesture:\n  %q\n  %q", CanonicalizeCommand(a), CanonicalizeCommand(b))
	}
}

func TestCanonicalizeCommand_KeepsDistinctActionsDistinct(t *testing.T) {
	if CanonicalizeCommand("nmap -sV 192.168.1.0") == CanonicalizeCommand("python3 /work/x.py") {
		t.Fatal("genuinely different actions must not share a canonical form")
	}
}

func TestDetectStuckByRepetition_FiresOnRepeatedGestureWithoutProgress(t *testing.T) {
	canon := CanonicalizeCommand(`grep -a "Success" /work/hydra_ssh_attack.log | head -30`)
	recent := []string{"nmap -sV N.N.N.N", canon, canon, canon}
	got, stuck := DetectStuckByRepetition(recent, 6, 8, 3, 6)
	if !stuck {
		t.Fatal("a gesture repeated 3x with no progress for 6 commands must be flagged")
	}
	if got != canon {
		t.Fatalf("should name the repeated gesture %q, got %q", canon, got)
	}
}

func TestDetectStuckByRepetition_DoesNotFireWhileProgressing(t *testing.T) {
	canon := CanonicalizeCommand("python3 /work/enum_final.py")
	recent := []string{canon, canon, canon}
	// cmdsSinceProgress below the threshold: new facts are still arriving.
	if _, stuck := DetectStuckByRepetition(recent, 2, 8, 3, 6); stuck {
		t.Fatal("a repeated gesture must not be flagged while progress is still being made")
	}
}

func TestDetectStuckByRepetition_DoesNotFireOnVariedCommands(t *testing.T) {
	recent := []string{"nmap -sV N.N.N.N", "curl http://N.N.N.N", "ls -la /work/*", "cat /work/*.txt", "whoami"}
	if _, stuck := DetectStuckByRepetition(recent, 10, 8, 3, 6); stuck {
		t.Fatal("genuinely different commands must not be flagged, even with no progress")
	}
}

func TestRecoveryDirectives_NameTheGestureAndEscalate(t *testing.T) {
	g := "python3 /work/*.py"
	if !strings.Contains(RepetitionRecoveryDirective(g), g) {
		t.Error("soft directive must name the gesture")
	}
	esc := EscalatedRecoveryDirective(g)
	if !strings.Contains(esc, g) || !strings.Contains(esc, "DISABLED") {
		t.Errorf("escalated directive must name the gesture and state it is disabled, got %q", esc)
	}
	if !strings.Contains(BlockedGestureMessage(g), g) {
		t.Error("blocked-gesture message must name the gesture")
	}
}
