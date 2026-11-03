package smallmodel

import (
	"reflect"
	"strings"
	"testing"
)

const fence = "```"

var recoveryTools = []string{"terminal", "code_result", "advice"}

func TestToolCallRecovery_RecoverToolCalls_PromotesCallsWrittenAsText(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantCalls []RecoveredToolCall
		wantRest  string
	}{
		{
			name:      "fenced json block as logged by the reflector",
			content:   fence + "json\n{\n  \"name\": \"code_result\",\n  \"arguments\": {\n    \"message\": \"Generated BOLA/IDOR scanning script\"\n  }\n}\n" + fence,
			wantCalls: []RecoveredToolCall{{Name: "code_result", Arguments: `{"message":"Generated BOLA/IDOR scanning script"}`}},
			wantRest:  "",
		},
		{
			name:      "prose before the fence is kept",
			content:   "Now let me create the IDOR testing framework:\n" + fence + "json\n" + `{"name": "terminal", "arguments": {"input": "mkdir -p /work/idor"}}` + "\n" + fence,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"mkdir -p /work/idor"}`}},
			wantRest:  "Now let me create the IDOR testing framework:",
		},
		{
			name:      "bare object inside prose",
			content:   `I will check who I am. {"name": "terminal", "arguments": {"input": "id"}} Then continue.`,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  "I will check who I am.  Then continue.",
		},
		{
			name:      "tool_call tags are dropped once empty",
			content:   "<tool_call>\n" + `{"name": "terminal", "arguments": {"input": "id"}}` + "\n</tool_call>",
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  "",
		},
		{
			name:      "raw newline inside a string value is escaped",
			content:   fence + "json\n{\"name\": \"terminal\", \"arguments\": {\"input\": \"echo a\necho b\"}}\n" + fence,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"echo a\necho b"}`}},
			wantRest:  "",
		},
		{
			name:      "unlabelled fence",
			content:   fence + "\n" + `{"name": "terminal", "arguments": {"input": "id"}}` + "\n" + fence,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  "",
		},
		{
			name:      "parameters key",
			content:   `{"name": "terminal", "parameters": {"input": "id"}}`,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  "",
		},
		{
			name:      "openai wrapper with string-encoded arguments",
			content:   `{"type": "function", "function": {"name": "terminal", "arguments": "{\"input\": \"id\"}"}}`,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  "",
		},
		{
			name: "two fences in order",
			content: fence + "json\n" + `{"name": "terminal", "arguments": {"input": "id"}}` + "\n" + fence +
				"\n\nand then\n\n" +
				fence + "json\n" + `{"name": "advice", "arguments": {"question": "what next?"}}` + "\n" + fence,
			wantCalls: []RecoveredToolCall{
				{Name: "terminal", Arguments: `{"input":"id"}`},
				{Name: "advice", Arguments: `{"question":"what next?"}`},
			},
			wantRest: "and then",
		},
		{
			name:    "array of calls is taken whole",
			content: fence + "json\n" + `[{"name": "terminal", "arguments": {"input": "id"}}, {"name": "terminal", "arguments": {"input": "uname -a"}}]` + "\n" + fence,
			wantCalls: []RecoveredToolCall{
				{Name: "terminal", Arguments: `{"input":"id"}`},
				{Name: "terminal", Arguments: `{"input":"uname -a"}`},
			},
			wantRest: "",
		},
		{
			name:      "array with an unknown tool keeps the unknown element",
			content:   `[{"name": "terminal", "arguments": {"input": "id"}}, {"name": "nmap", "arguments": {}}]`,
			wantCalls: []RecoveredToolCall{{Name: "terminal", Arguments: `{"input":"id"}`}},
			wantRest:  `[, {"name": "nmap", "arguments": {}}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCalls, gotRest := RecoverToolCalls(tc.content, recoveryTools)
			if !reflect.DeepEqual(gotCalls, tc.wantCalls) {
				t.Errorf("calls = %#v, want %#v", gotCalls, tc.wantCalls)
			}
			if gotRest != tc.wantRest {
				t.Errorf("rest = %q, want %q", gotRest, tc.wantRest)
			}
		})
	}
}

func TestToolCallRecovery_RecoverToolCalls_LeavesEverythingElseInContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		tools   []string
	}{
		{name: "unknown tool name", content: `{"name": "nmap", "arguments": {"target": "10.0.0.5"}}`, tools: recoveryTools},
		{name: "arguments is a plain string", content: `{"name": "terminal", "arguments": "id"}`, tools: recoveryTools},
		{name: "arguments is an array", content: `{"name": "terminal", "arguments": ["id"]}`, tools: recoveryTools},
		{name: "arguments missing", content: `{"name": "terminal"}`, tools: recoveryTools},
		{name: "name is null", content: `{"name": null, "arguments": {}}`, tools: recoveryTools},
		{name: "unclosed object", content: `{"name": "terminal", "arguments": {"input": "id"}`, tools: recoveryTools},
		{name: "json that is not a call", content: `Banner: {"name": "OpenSSH", "version": "8.9"}`, tools: recoveryTools},
		{name: "prose with brackets", content: `The "name" field goes in {braces} and [brackets].`, tools: recoveryTools},
		{name: "surrounding whitespace is preserved", content: "  {\"name\": \"nmap\", \"arguments\": {}}\n", tools: recoveryTools},
		{name: "no tools offered", content: `{"name": "terminal", "arguments": {"input": "id"}}`, tools: nil},
		{name: "content over the size cap", content: strings.Repeat("a", 64*1024) + `{"name": "terminal", "arguments": {"input": "id"}}`, tools: recoveryTools},
		{name: "call after degenerate text that exhausts the scan budget", content: strings.Repeat(`{"name"`, 9000) + `{"name": "terminal", "arguments": {"input": "id"}}`, tools: recoveryTools},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCalls, gotRest := RecoverToolCalls(tc.content, tc.tools)
			if gotCalls != nil {
				t.Errorf("calls = %#v, want nil", gotCalls)
			}
			if gotRest != tc.content {
				t.Errorf("rest = %q, want the content unchanged", gotRest)
			}
		})
	}
}
