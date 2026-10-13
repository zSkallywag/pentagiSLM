package smallmodel

import (
	"strings"
	"testing"
)

func TestStrategyGate_PassesNonBruteForce(t *testing.T) {
	if gated, _ := StrategyGate("nmap -sV -p22 192.168.1.0", nil); gated {
		t.Fatal("a non-brute-force command must never be gated")
	}
}

func TestStrategyGate_IgnoresSingleTargetedAttempt(t *testing.T) {
	// hydra with one -l/-p pair and no list is a single guess, not a wordlist run.
	if gated, _ := StrategyGate("hydra -l student -p Secret ssh://192.168.1.0", nil); gated {
		t.Fatal("a single targeted credential attempt must not be gated")
	}
}

func TestStrategyGate_GatesWordlistBeforeRecon(t *testing.T) {
	gated, msg := StrategyGate("hydra -L users.txt -P rockyou.txt ssh://192.168.1.0", nil)
	if !gated {
		t.Fatal("a wordlist brute-force before any enumeration must be gated")
	}
	if !strings.Contains(msg, "enumeration") {
		t.Fatalf("message should point to missing enumeration, got %q", msg)
	}
}

func TestStrategyGate_GatesWhenCredentialKnown(t *testing.T) {
	facts := []string{"22/tcp open: ssh", CredentialFactPrefix + "student:Secret"}
	gated, msg := StrategyGate("hydra -L users.txt -P rockyou.txt ssh://192.168.1.0", facts)
	if !gated {
		t.Fatal("brute-force while a credential is already known must be gated")
	}
	if !strings.Contains(msg, "student:Secret") {
		t.Fatalf("message should replay the known credential fact, got %q", msg)
	}
}

func TestStrategyGate_AllowsLastResortAfterReconWithoutCredential(t *testing.T) {
	facts := []string{"22/tcp open: ssh (OpenSSH 8.2)"}
	if gated, _ := StrategyGate("hydra -L users.txt -P rockyou.txt ssh://192.168.1.0", facts); gated {
		t.Fatal("after recon with no credential in hand, a wordlist attack is a valid last resort")
	}
}

func TestIsWordlistBruteForce_DetectsToolPlusList(t *testing.T) {
	yes := []string{
		"hydra -L u.txt -P p.txt ssh://h",
		"medusa -U users -P /usr/share/wordlists/rockyou.txt -h h -M ssh",
		"ncrack --user root -P pass.txt ssh://h",
		"patator ssh_login host=h user=root password=FILE0 0=words.txt --wordlist",
	}
	for _, c := range yes {
		if !IsWordlistBruteForce(c) {
			t.Errorf("expected wordlist brute-force: %q", c)
		}
	}
	no := []string{
		"nmap -sV 192.168.1.0",
		"hydra -l root -p toor ssh://h",
		"ssh student@192.168.1.0",
	}
	for _, c := range no {
		if IsWordlistBruteForce(c) {
			t.Errorf("did not expect wordlist brute-force: %q", c)
		}
	}
}
