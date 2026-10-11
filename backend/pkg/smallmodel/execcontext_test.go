package smallmodel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppendExecContext_StampsSandboxContextAndKeepsOutput(t *testing.T) {
	out := AppendExecContext("uid=0(root) gid=0(root) groups=0(root)")

	assert.Contains(t, out, "uid=0(root)", "original output is preserved")
	assert.Contains(t, out, "PentAGI local sandbox", "the sandbox context is stamped")
	assert.True(t, strings.HasSuffix(out, ExecContextNote), "the note is appended at the end")
}
