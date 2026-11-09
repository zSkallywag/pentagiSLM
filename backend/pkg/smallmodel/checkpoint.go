package smallmodel

import (
	"fmt"
	"regexp"
	"strings"
)

// BudgetCheckpoints are the percentages of a subtask's command budget at which
// the adviser reviews the subtask. Reaching the hard cap is the latest moment a
// stuck subtask can be stopped; asking earlier spends the review while the
// budget left is still worth redirecting.
var BudgetCheckpoints = []int{40, 75}

// CheckpointVerdict is the decision a budget checkpoint ends in. A small model is
// far more reliable choosing among three named options than writing its own plan,
// so the review is a closed choice.
type CheckpointVerdict string

const (
	// CheckpointContinue keeps the subtask running, on the approach the adviser names.
	CheckpointContinue CheckpointVerdict = "CONTINUE"
	// CheckpointFinish ends the subtask's commands: what was found is enough to report.
	CheckpointFinish CheckpointVerdict = "FINISH"
	// CheckpointBlocked ends the subtask's commands: it cannot succeed as defined.
	CheckpointBlocked CheckpointVerdict = "BLOCKED"
)

// Closes reports whether the verdict ends the subtask's commands.
func (v CheckpointVerdict) Closes() bool {
	return v == CheckpointFinish || v == CheckpointBlocked
}

var reVerdict = regexp.MustCompile(`(?im)^[\s*_#>-]*VERDICT\s*:\s*[*_]*\s*(CONTINUE|FINISH|BLOCKED)\b`)

// ParseCheckpointVerdict reads the verdict from an adviser's answer: the last
// "VERDICT: <option>" line, tolerating the markdown a model wraps it in. ok is
// false when there is none, and the caller then keeps the subtask running — a
// garbled answer must not end a subtask that may be on track.
func ParseCheckpointVerdict(answer string) (CheckpointVerdict, bool) {
	matches := reVerdict.FindAllStringSubmatch(answer, -1)
	if len(matches) == 0 {
		return "", false
	}
	return CheckpointVerdict(strings.ToUpper(matches[len(matches)-1][1])), true
}

// CheckpointQuestion is appended to the execution-monitor question at a budget
// checkpoint. It states how much of the budget is gone and requires the answer
// to end in exactly one of the three verdicts.
func CheckpointQuestion(percent, count, cap int) string {
	return fmt.Sprintf(
		"BUDGET CHECKPOINT: this subtask has used %d%% of its command budget (%d of %d terminal commands). "+
			"Decide how it goes on, and end your answer with exactly one line, one of:\n"+
			"VERDICT: CONTINUE — it is making real progress; name the one approach to pursue next and what to stop doing.\n"+
			"VERDICT: FINISH — what has been found is enough; it should report its findings now.\n"+
			"VERDICT: BLOCKED — it cannot succeed as defined; it should report the blocker now.",
		percent, count, cap,
	)
}

// CheckpointNote is what the agent receives after the call that reached a
// checkpoint: the verdict first, so it is not lost under the analysis, then the
// adviser's answer.
func CheckpointNote(percent int, verdict CheckpointVerdict, advice string) string {
	var head string
	switch verdict {
	case CheckpointFinish:
		head = "the adviser judged that what you have found is enough. Commands are no longer available " +
			"for this subtask: report your findings and complete it now."
	case CheckpointBlocked:
		head = "the adviser judged that this subtask cannot succeed as defined. Commands are no longer " +
			"available for it: report the blocker and what you found, and complete it now."
	default:
		head = "the adviser judged that the subtask may continue. Follow the approach it names and drop " +
			"what it tells you to stop."
	}
	return fmt.Sprintf("BUDGET CHECKPOINT (%d%%, verdict %s): %s\n\n%s", percent, verdict, head, advice)
}
