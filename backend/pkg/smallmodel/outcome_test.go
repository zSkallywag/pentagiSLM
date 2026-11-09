package smallmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutcome_LooksFailed_ReadsOnlyDeterministicFailureMarkers(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "a command that exited zero", output: "total 0\n[exit code: 0]", want: false},
		{name: "a command that exited non-zero", output: "curl: (6) Could not resolve host\n[exit code: 6]", want: true},
		{name: "a missing binary", output: "bash: sshpass: command not found\n[exit code: 127]", want: true},
		{name: "a JSON error body", output: `{"code":400,"error":"invalid JSON body: json: cannot unmarshal string"}`, want: true},
		{name: "a JSON status as a string", output: `{"status": "404", "message": "Not Found"}`, want: true},
		{name: "an HTTP error status line", output: "HTTP/1.1 403 Forbidden\nContent-Type: text/plain", want: true},
		{name: "an HTTP success status line", output: "HTTP/2 200\nserver: CloudFront", want: false},
		{name: "curl's http_code on a line of its own", output: "<html>gone</html>\n404\n[exit code: 0]", want: true},
		{name: "a number that only starts like a status", output: "listing 4040 results\n[exit code: 0]", want: false},
		{name: "a JSON success code", output: `{"error_count": 0, "code": 200}`, want: false},
		{name: "the tool's own error wrapper", output: "terminal tool 'file' handled with error: failed to copy file", want: true},
		{name: "a missing path", output: "cat: /work/out.json: No such file or directory\n[exit code: 1]", want: true},
		{name: "prose that only mentions an error", output: "the page documents the error codes it returns", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LooksFailed(tt.output))
		})
	}
}
