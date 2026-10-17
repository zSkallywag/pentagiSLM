package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/vxcontrol/langchaingo/llms"

	"pentagi/pkg/cast"
	"pentagi/pkg/smallmodel"
	"pentagi/pkg/tools"
)

// smallModelRecoveryEscalateAfter is how many turns in a row the repetition
// recovery directive may fire on the same gesture before the gesture is disabled
// and the wording escalated — i.e. how many times the model may ignore the nudge.
const smallModelRecoveryEscalateAfter = 3

// smallModelEnabled reports whether the small-model profile is active.
func (fp *flowProvider) smallModelEnabled() bool {
	return fp.cfg != nil && fp.cfg.SmallModelMode
}

// smallModelVerifier returns the lazily-built verifier configured from the
// authorized scope and the operator's denied-command list.
func (fp *flowProvider) smallModelVerifier() *smallmodel.Verifier {
	fp.mx.Lock()
	defer fp.mx.Unlock()
	if fp.smVerifier == nil {
		fp.smVerifier = smallmodel.NewVerifier(fp.cfg.SmallModelScope, fp.cfg.SmallModelDeniedCommands)
	}
	return fp.smVerifier
}

// smallModelState returns the per-flow structured-state store.
func (fp *flowProvider) smallModelState() *smallmodel.Store {
	fp.mx.Lock()
	defer fp.mx.Unlock()
	if fp.smState == nil {
		fp.smState = smallmodel.NewStore()
	}
	return fp.smState
}

// smallModelBudget builds the token budget from config.
func (fp *flowProvider) smallModelBudget() smallmodel.Budget {
	return smallmodel.Budget{
		CtxWindow:     fp.cfg.SmallModelCtxWindow,
		Percent:       fp.cfg.SmallModelCtxBudgetPercent,
		BytesPerToken: fp.cfg.SmallModelBytesPerToken,
	}
}

// sanitizeToolArgs repairs malformed JSON arguments from a small model before
// execution. It only substitutes the sanitized form when that form is valid
// JSON, so a value it cannot fix is left for the normal execution-error repair
// path rather than being blanked out.
func (fp *flowProvider) sanitizeToolArgs(funcArgs json.RawMessage) json.RawMessage {
	if !fp.smallModelEnabled() {
		return funcArgs
	}
	sanitized := cast.SanitizeJSONControlChars(string(funcArgs))
	if sanitized != string(funcArgs) && json.Valid([]byte(sanitized)) {
		return json.RawMessage(sanitized)
	}
	return funcArgs
}

// verifyToolCall classifies a proposed tool call before execution. When the call
// is not allowed it returns a tool response to feed back to the model and blocked
// set, so the caller skips execution and keeps the agent inside the authorized
// scope. It applies two guards to commands: the scope/destructive verifier, and
// the strategy gate that holds back a premature wordlist brute-force. Other tools
// pass through.
func (fp *flowProvider) verifyToolCall(funcName string, funcArgs json.RawMessage, taskID, subtaskID *int64) (string, bool) {
	if !fp.smallModelEnabled() || !fp.cfg.SmallModelVerifierEnabled {
		return "", false
	}
	if funcName != tools.TerminalToolName {
		return "", false
	}

	// Per-subtask command budget: the hardest cap, checked before anything else
	// so a runaway "do not stop" subtask is forced to conclude.
	if subtaskID != nil && fp.cfg.SmallModelSubtaskMaxCommands > 0 {
		if n := fp.smallModelState().SubtaskCommands(*subtaskID); n >= fp.cfg.SmallModelSubtaskMaxCommands {
			logrus.WithFields(logrus.Fields{
				"flow": fp.flowID, "subtask": *subtaskID, "guard": "subtask_budget", "count": n,
			}).Warn("small-model guard exhausted the subtask command budget")
			return smallmodel.SubtaskBudgetMessage(n, fp.cfg.SmallModelSubtaskMaxCommands), true
		}
	}

	var action struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(funcArgs, &action); err != nil || action.Input == "" {
		return "", false
	}

	if verdict := fp.smallModelVerifier().CheckCommand(action.Input); verdict.Decision != smallmodel.Allow {
		return fmt.Sprintf(
			"BLOCKED by pre-execution verifier: %s. The command was not run. "+
				"If this is in scope and intended, it needs explicit operator confirmation; "+
				"otherwise choose an in-scope, non-destructive action.",
			verdict.Reason,
		), true
	}

	// Hold back a premature wordlist brute-force once per task, replaying the
	// facts so the agent uses a known credential or finishes enumeration first.
	if taskID != nil {
		key := smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID}
		// A gesture disabled by the recovery escalation is rejected before it
		// runs, so an ignored loop cannot continue.
		canon := smallmodel.CanonicalizeCommand(action.Input)
		if fp.smallModelState().IsGestureBlocked(key, canon) {
			logrus.WithFields(logrus.Fields{
				"flow": fp.flowID, "task": *taskID, "guard": "blocked_gesture", "gesture": canon,
			}).Warn("small-model guard rejected a disabled gesture")
			return smallmodel.BlockedGestureMessage(canon), true
		}
		if !fp.smallModelState().StrategyGateFired(key) {
			if gated, msg := smallmodel.StrategyGate(action.Input, fp.smallModelState().Facts(key)); gated {
				fp.smallModelState().MarkStrategyGateFired(key)
				logrus.WithFields(logrus.Fields{
					"flow": fp.flowID, "task": *taskID, "guard": "strategy_gate",
				}).Warn("small-model guard held back a premature wordlist brute-force")
				return msg, true
			}
		}
	}

	return "", false
}

// compactToolResponse pre-processes a raw tool response for a small model: it
// parses known formats, keeps head and tail of long output, records the
// extracted facts in the structured state so they survive summarization, and —
// when a classifier is configured — records a grounded failed attempt so the
// state carries what actually failed rather than the model's narrative.
func (fp *flowProvider) compactToolResponse(ctx context.Context, funcName, response string, taskID *int64) string {
	if !fp.smallModelEnabled() {
		return response
	}

	compacted := smallmodel.CompactOutput(funcName, response, fp.cfg.SmallModelToolOutputMaxBytes)
	if taskID != nil {
		key := smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID}
		if len(compacted.Facts) > 0 {
			fp.smallModelState().MergeFacts(key, compacted.Facts)
		}
		// Detect on the raw output: a "command not found" line is short and
		// never truncated, but compaction could drop it from a large output, and
		// losing this signal is exactly what makes a small model loop on an
		// absent tool instead of installing it.
		if funcName == tools.TerminalToolName {
			for _, name := range smallmodel.DetectMissingTools(response) {
				fp.smallModelState().AddFacts(key, smallmodel.MissingToolFact(name))
			}
		}
		if outcome, score, ok := fp.smallModelJudgeOutcome(ctx, compacted.Summary); ok && outcome == "failure" && score >= 0.7 {
			fp.smallModelState().AddFailedTry(key, fmt.Sprintf("%s failed: %s", funcName, firstLine(compacted.Summary)))
		}
	}

	// Stamp where the command ran so the model cannot mistake a local sandbox
	// command (root) for one executed on a remote target.
	if funcName == tools.TerminalToolName {
		return smallmodel.AppendExecContext(compacted.Summary)
	}
	return compacted.Summary
}

// smallModelClassifier returns the lazily-built decision-sidecar client.
func (fp *flowProvider) smallModelClassifier() *smallmodel.Classifier {
	fp.mx.Lock()
	defer fp.mx.Unlock()
	if fp.smClassifier == nil {
		fp.smClassifier = smallmodel.NewClassifier(fp.cfg.SmallModelClassifierURL)
	}
	return fp.smClassifier
}

// smallModelJudgeOutcome classifies a tool output as "success" or "failure" with
// a confidence. ok is false when classification is disabled or the sidecar did
// not answer, so the caller never records an ungrounded verdict.
func (fp *flowProvider) smallModelJudgeOutcome(ctx context.Context, output string) (string, float64, bool) {
	if !fp.smallModelEnabled() || fp.cfg.SmallModelClassifierURL == "" || strings.TrimSpace(output) == "" {
		return "", 0, false
	}
	label, score, err := fp.smallModelClassifier().Choose(
		ctx, output, []string{"success", "failure"}, "This command output indicates a {}.",
	)
	if err != nil {
		return "", 0, false
	}
	return label, score, true
}

// firstLine returns the first line of s, trimmed and capped, for a compact state note.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// smallModelStateBlock returns the structured-state block to prepend to a
// prompt, or an empty string when the profile is off or there is nothing yet.
// When the agent is detected looping on a failing approach, a recovery directive
// is prepended so it switches mechanism instead of varying a broken one.
func (fp *flowProvider) smallModelStateBlock(taskID *int64) string {
	if !fp.smallModelEnabled() || taskID == nil {
		return ""
	}
	key := smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID}
	block := fp.smallModelState().Block(key)
	if block == "" {
		return ""
	}
	// Trigger 1: recent grounded failures that resemble each other — a loop of
	// failing attempts, caught through the success/failure classifier.
	if smallmodel.DetectStuck(fp.smallModelState().FailedTries(key), 4, 0.5) {
		logrus.WithFields(logrus.Fields{
			"flow": fp.flowID, "task": *taskID, "trigger": "failure_similarity",
		}).Warn("small-model recovery directive armed")
		return smallmodel.RecoveryDirective + "\n\n" + block
	}
	// Trigger 2: one canonical action repeated while no new fact is learned — a
	// loop of any kind (brute-force grind, log re-reading, script rewriting),
	// caught deterministically without the classifier.
	recent, sinceProgress := fp.smallModelState().RepetitionState(key)
	if canon, stuck := smallmodel.DetectStuckByRepetition(recent, sinceProgress, 8, 3, 6); stuck {
		count := fp.smallModelState().NoteRecoveryArmed(key, canon)
		logrus.WithFields(logrus.Fields{
			"flow": fp.flowID, "task": *taskID, "trigger": "repetition", "gesture": canon, "ignored": count - 1,
		}).Warn("small-model recovery directive armed")
		// The soft nudge was ignored this many turns in a row: disable the
		// gesture outright so the pre-execution guard rejects it, and escalate
		// the wording. The execution monitor (on under the profile) hands the
		// hard case to the adviser in parallel.
		if count >= smallModelRecoveryEscalateAfter {
			fp.smallModelState().BlockGesture(key, canon)
			return smallmodel.EscalatedRecoveryDirective(canon) + "\n\n" + block
		}
		return smallmodel.RepetitionRecoveryDirective(canon) + "\n\n" + block
	}
	return block
}

// recordTerminalCommand files a terminal command's canonical form into the
// per-task loop-detection ring before it executes, so DetectStuckByRepetition
// can see a repeated gesture on the next turn.
func (fp *flowProvider) recordTerminalCommand(funcName string, funcArgs json.RawMessage, taskID, subtaskID *int64) {
	if !fp.smallModelEnabled() || taskID == nil || funcName != tools.TerminalToolName {
		return
	}
	var action struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(funcArgs, &action); err != nil || action.Input == "" {
		return
	}
	key := smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID}
	fp.smallModelState().RecordCommand(key, smallmodel.CanonicalizeCommand(action.Input))
	if subtaskID != nil {
		fp.smallModelState().IncrSubtaskCommands(*subtaskID)
	}
}

// smallModelExamples lazily loads the few-shot example set from the configured
// file. It records that it tried so a missing or malformed file is not re-read
// every turn, and returns nil on any error (few-shot is then simply skipped).
func (fp *flowProvider) smallModelExamples() *smallmodel.ExampleSet {
	fp.mx.Lock()
	defer fp.mx.Unlock()
	if fp.smFewshotTried {
		return fp.smFewshot
	}
	fp.smFewshotTried = true
	set, err := smallmodel.LoadExamples(fp.cfg.SmallModelFewshotFile)
	if err != nil {
		logrus.WithError(err).Warn("failed to load small-model few-shot examples, continuing without them")
		return nil
	}
	fp.smFewshot = set
	return set
}

// smallModelFewshotBlock returns the few-shot block most relevant to query, or
// an empty string when the profile is off, no file is configured, or nothing
// matches.
func (fp *flowProvider) smallModelFewshotBlock(query string) string {
	if !fp.smallModelEnabled() || fp.cfg.SmallModelFewshotFile == "" || query == "" {
		return ""
	}
	set := fp.smallModelExamples()
	if set == nil {
		return ""
	}
	return set.Block(query, fp.cfg.SmallModelFewshotK)
}

// smallModelCompactIfOverBudget compacts the chain when it exceeds the token
// budget, independently of the summarizer's own byte thresholds. It returns the
// chain unchanged on any error so it can never make the chain worse.
func (fp *flowProvider) smallModelCompactIfOverBudget(
	ctx context.Context,
	handler tools.SummarizeHandler,
	chain []llms.MessageContent,
) ([]llms.MessageContent, bool) {
	if !fp.smallModelEnabled() || fp.summarizer == nil {
		return chain, false
	}
	if !fp.smallModelBudget().Exceeded(chain) {
		return chain, false
	}
	compacted, err := fp.summarizer.SummarizeChain(ctx, handler, chain, fp.tcIDTemplate)
	if err != nil {
		return chain, false
	}
	return compacted, true
}
