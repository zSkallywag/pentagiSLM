package smallmodel

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectMissingTools_ParsesShellForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"dash sh form", "sh: 1: sshpass: not found", []string{"sshpass"}},
		{"bash line form", "bash: line 1: sshpass: command not found", []string{"sshpass"}},
		{"bare form", "expect: command not found", []string{"expect"}},
		{"zsh inverted form", "zsh: command not found: nmap", []string{"nmap"}},
		{"ansi coloured", "\x1b[92msh: 1: sshpass: not found \x1b[0m", []string{"sshpass"}},
		{"several distinct", "sh: 1: sshpass: not found\nbash: line 2: pexpect: command not found", []string{"sshpass", "pexpect"}},
		{"deduped", "sshpass: command not found\nsshpass: command not found", []string{"sshpass"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectMissingTools(c.in); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("DetectMissingTools(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestDetectMissingTools_RejectsMistypedCommands(t *testing.T) {
	// The shell reports a mistyped "user:pass" command as not found too; it is
	// not a package and must never become an install target.
	if got := DetectMissingTools("bash: line 1: student:password: command not found"); got != nil {
		t.Fatalf("expected no tool from a colon-bearing command, got %v", got)
	}
}

func TestDetectMissingTools_IgnoresUnrelatedOutput(t *testing.T) {
	if got := DetectMissingTools("22/tcp open ssh\nfile not found: /etc/shadow"); got != nil {
		// "file not found: ..." is captured by the prefix rule only for
		// "command not found:"; a path here must not be mistaken for a tool.
		t.Fatalf("expected no tool from unrelated 'not found' text, got %v", got)
	}
}

func TestMissingToolFact_IsActionableAndNamesInstall(t *testing.T) {
	fact := MissingToolFact("sshpass")
	for _, want := range []string{"sshpass", "apt-get install", "if you still need it"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact %q missing %q", fact, want)
		}
	}
}
