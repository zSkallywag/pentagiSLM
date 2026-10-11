package smallmodel

import "testing"

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
