package smallmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A small model often writes a correct tool call as message text — in a ```json
// fence, inside <tool_call> tags, or bare — instead of through the structured
// tool-call channel. PentAGI treats that reply as prose and hands it to the
// reflector, whose retries the same model fails the same way until the chain
// aborts. Recovering the call from the text breaks that loop at the source.

// RecoveredToolCall is a tool call found in message text. Arguments is a compact
// JSON object.
type RecoveredToolCall struct {
	Name      string
	Arguments string
}

// The scan tries a JSON parse at every candidate bracket, so on degenerate text —
// a model repeating `{"name": ...` until max_tokens — its cost grows with the
// square of the input. The work budget bounds that; a call written after such
// text may then be missed.
const (
	maxRecoveryContentBytes = 64 * 1024
	maxRecoveryScanBytes    = 2 << 20
)

var (
	emptyFence       = regexp.MustCompile("(?s)```[A-Za-z0-9_-]*\\s*```")
	emptyToolCallTag = regexp.MustCompile(`(?s)<tool_call>\s*</tool_call>`)
)

// RecoverToolCalls returns, in order, the tool calls written as JSON in content,
// and content with them — and any fence or <tool_call> tag they leave empty —
// removed and trimmed.
//
// A JSON object is a call only when its "name" is one of tools and its
// "arguments" (or "parameters") is a JSON object or a JSON-encoded string of
// one; the OpenAI {"type": "function", "function": {...}} wrapper is unwrapped.
// An array is taken whole when every element is a call; otherwise its elements
// are considered one by one. Anything else stays in content. With no call found
// it returns nil and content unchanged.
func RecoverToolCalls(content string, tools []string) ([]RecoveredToolCall, string) {
	if len(tools) == 0 || len(content) > maxRecoveryContentBytes || !strings.Contains(content, `"name"`) {
		return nil, content
	}

	known := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		known[name] = struct{}{}
	}

	var (
		calls  []RecoveredToolCall
		rest   strings.Builder
		last   int
		budget = maxRecoveryScanBytes
	)
	for i := 0; i < len(content) && budget > 0; i++ {
		if !isCandidateStart(content, i) {
			continue
		}
		end, sanitized, scanned, ok := jsonValueSpan(content, i)
		budget -= scanned
		if !ok {
			continue
		}
		found, ok := callsIn([]byte(sanitized), known)
		if !ok {
			continue
		}
		calls = append(calls, found...)
		rest.WriteString(content[last:i])
		last = end
		i = end - 1
	}
	if len(calls) == 0 {
		return nil, content
	}
	rest.WriteString(content[last:])

	out := emptyFence.ReplaceAllString(rest.String(), "")
	out = emptyToolCallTag.ReplaceAllString(out, "")
	return calls, strings.TrimSpace(out)
}

func isCandidateStart(content string, i int) bool {
	var want byte
	switch content[i] {
	case '{':
		want = '"'
	case '[':
		want = '{'
	default:
		return false
	}
	next := strings.TrimLeft(content[i+1:], " \t\r\n")
	return next != "" && next[0] == want
}

// jsonValueSpan finds the end of the object or array opening at content[start],
// ignoring brackets inside strings, and reports how many bytes it read. Raw
// control characters inside strings are escaped in the returned copy: a model
// writes literal newlines into a string value, which encoding/json rejects.
func jsonValueSpan(content string, start int) (end int, sanitized string, scanned int, ok bool) {
	var (
		buf      strings.Builder
		depth    int
		inString bool
		escaped  bool
	)
	for i := start; i < len(content); i++ {
		b := content[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			case b < 0x20:
				buf.WriteString(escapeControl(b))
				continue
			}
			buf.WriteByte(b)
			continue
		}

		buf.WriteByte(b)
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, buf.String(), i + 1 - start, true
			}
		}
	}
	return 0, "", len(content) - start, false
}

func escapeControl(b byte) string {
	switch b {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	default:
		return fmt.Sprintf(`\u%04x`, b)
	}
}

func callsIn(raw []byte, known map[string]struct{}) ([]RecoveredToolCall, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}

	if raw[0] == '[' {
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil || len(elems) == 0 {
			return nil, false
		}
		calls := make([]RecoveredToolCall, 0, len(elems))
		for _, elem := range elems {
			call, ok := asCall(elem, known)
			if !ok {
				return nil, false
			}
			calls = append(calls, call)
		}
		return calls, true
	}

	call, ok := asCall(raw, known)
	if !ok {
		return nil, false
	}
	return []RecoveredToolCall{call}, true
}

func asCall(raw json.RawMessage, known map[string]struct{}) (RecoveredToolCall, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return RecoveredToolCall{}, false
	}

	if fn, ok := obj["function"]; ok && obj["name"] == nil {
		return asCall(fn, known)
	}

	var name string
	if json.Unmarshal(obj["name"], &name) != nil {
		return RecoveredToolCall{}, false
	}
	if _, ok := known[name]; !ok {
		return RecoveredToolCall{}, false
	}

	args, ok := obj["arguments"]
	if !ok {
		args, ok = obj["parameters"]
	}
	if !ok {
		return RecoveredToolCall{}, false
	}
	compact, ok := argumentsObject(args)
	if !ok {
		return RecoveredToolCall{}, false
	}

	return RecoveredToolCall{Name: name, Arguments: compact}, true
}

// argumentsObject accepts a JSON object, or a string holding one — the OpenAI
// wire form encodes arguments as a string.
func argumentsObject(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return "", false
		}
		raw = bytes.TrimSpace([]byte(encoded))
	}
	if len(raw) == 0 || raw[0] != '{' {
		return "", false
	}

	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return "", false
	}
	return buf.String(), true
}
