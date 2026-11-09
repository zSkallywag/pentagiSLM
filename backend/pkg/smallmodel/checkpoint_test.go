package smallmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckpoint_ParseCheckpointVerdict_ReadsTheLastVerdictLine(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   CheckpointVerdict
		ok     bool
	}{
		{name: "a plain verdict line", answer: "No new endpoint in 20 commands.\nVERDICT: FINISH", want: CheckpointFinish, ok: true},
		{name: "a verdict in bold", answer: "It cannot work.\n**VERDICT: BLOCKED**", want: CheckpointBlocked, ok: true},
		{name: "a verdict in a list item", answer: "- VERDICT: CONTINUE — send listings as objects", want: CheckpointContinue, ok: true},
		{name: "a verdict in lower case", answer: "verdict: continue", want: CheckpointContinue, ok: true},
		{name: "the last of two verdicts", answer: "VERDICT: CONTINUE\nOn reflection:\nVERDICT: FINISH", want: CheckpointFinish, ok: true},
		{name: "an answer with no verdict", answer: "Keep going, the verdict is clear.", ok: false},
		{name: "an option that is not offered", answer: "VERDICT: MAYBE", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCheckpointVerdict(tt.answer)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckpoint_CheckpointQuestion_OffersExactlyTheThreeVerdicts(t *testing.T) {
	q := CheckpointQuestion(40, 60, 150)

	assert.Contains(t, q, "used 40% of its command budget (60 of 150 terminal commands)")
	assert.Contains(t, q, "\nVERDICT: CONTINUE — ")
	assert.Contains(t, q, "\nVERDICT: FINISH — ")
	assert.Contains(t, q, "\nVERDICT: BLOCKED — ")
}

func TestCheckpoint_CheckpointNote_LeadsWithTheVerdict(t *testing.T) {
	finish := CheckpointNote(40, CheckpointFinish, "Enough is known.\nVERDICT: FINISH")
	assert.Contains(t, finish, "BUDGET CHECKPOINT (40%, verdict FINISH): ")
	assert.Contains(t, finish, "Commands are no longer available")
	assert.Contains(t, finish, "\n\nEnough is known.\nVERDICT: FINISH")

	assert.Contains(t, CheckpointNote(75, CheckpointBlocked, "x"), "report the blocker")

	cont := CheckpointNote(40, CheckpointContinue, "Send listings as objects.")
	assert.Contains(t, cont, "BUDGET CHECKPOINT (40%, verdict CONTINUE): ")
	assert.NotContains(t, cont, "no longer available", "a subtask told to continue keeps its commands")
}
