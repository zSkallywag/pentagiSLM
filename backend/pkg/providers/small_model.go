package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/vxcontrol/langchaingo/llms"

	"pentagi/pkg/cast"
	"pentagi/pkg/csum"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/smallmodel"
	"pentagi/pkg/templates"
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

// smallModelRecoverToolCalls promotes tool calls the model wrote as JSON in its
// message text to real tool calls, returning the remaining text. Recovered calls
// still pass through the pre-execution guards like any other call.
func (fp *flowProvider) smallModelRecoverToolCalls(
	logger *logrus.Entry,
	content string,
	executor tools.ContextToolsExecutor,
) (string, []llms.ToolCall) {
	if !fp.smallModelEnabled() || strings.TrimSpace(content) == "" {
		return content, nil
	}

	defs := executor.Tools()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		if def.Function != nil {
			names = append(names, def.Function.Name)
		}
	}

	recovered, rest := smallmodel.RecoverToolCalls(content, names)
	if len(recovered) == 0 {
		return content, nil
	}

	calls := make([]llms.ToolCall, 0, len(recovered))
	recoveredNames := make([]string, 0, len(recovered))
	for _, rc := range recovered {
		calls = append(calls, llms.ToolCall{
			ID:   templates.GenerateFromPattern(fp.tcIDTemplate, rc.Name),
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      rc.Name,
				Arguments: rc.Arguments,
			},
		})
		recoveredNames = append(recoveredNames, rc.Name)
	}

	logger.WithFields(logrus.Fields{
		"tools": recoveredNames,
		"count": len(calls),
	}).Warn("recovered tool call from content")

	return rest, calls
}

// smallModelBudget builds the token budget for the role opt. The window is the
// one the role's model is served with when the provider reports it, so a role on
// a small local model compacts at that model's ceiling while a role on a large
// cloud model keeps its context; SMALL_MODEL_CTX_WINDOW covers a model whose
// window is unknown. The answer the role asks for and the tool schemas it sends
// share the window with the chain, so their room is reserved.
func (fp *flowProvider) smallModelBudget(opt pconfig.ProviderOptionsType, schemas []llms.Tool) smallmodel.Budget {
	window := fp.cfg.SmallModelCtxWindow
	if fp.Provider != nil {
		if limits := provider.ModelLimitsFor(fp.Provider, opt); limits.ContextWindow != nil && *limits.ContextWindow > 0 {
			window = *limits.ContextWindow
		}
	}

	return smallmodel.Budget{
		CtxWindow:     window,
		Percent:       fp.cfg.SmallModelCtxBudgetPercent,
		BytesPerToken: fp.cfg.SmallModelBytesPerToken,
		ReserveTokens: fp.smallModelAnswerTokens(opt) +
			smallmodel.EstimateToolTokens(schemas, fp.cfg.SmallModelBytesPerToken),
	}
}

// smallModelAnswerTokens is the max_tokens a call for opt carries, the provider
// defaults included, or zero when none is set.
func (fp *flowProvider) smallModelAnswerTokens(opt pconfig.ProviderOptionsType) int {
	if fp.Provider == nil {
		return 0
	}
	var applied llms.CallOptions
	for _, option := range fp.GetProviderConfig().GetOptionsForType(opt) {
		option(&applied)
	}
	if applied.MaxTokens == nil || *applied.MaxTokens < 0 {
		return 0
	}
	return *applied.MaxTokens
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
	if n, spent := fp.smallModelSubtaskBudgetSpent(subtaskID); spent {
		logrus.WithFields(logrus.Fields{
			"flow": fp.flowID, "subtask": *subtaskID, "guard": "subtask_budget", "count": n,
		}).Warn("small-model guard exhausted the subtask command budget")
		return smallmodel.SubtaskBudgetMessage(n, fp.cfg.SmallModelSubtaskMaxCommands), true
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

// smallModelSubtaskBudgetSpent reports the terminal commands a subtask has run and
// whether that has reached its budget, the point from which commands are refused
// and the agent is left only the tools to finish with.
func (fp *flowProvider) smallModelSubtaskBudgetSpent(subtaskID *int64) (int, bool) {
	if !fp.smallModelEnabled() || !fp.cfg.SmallModelVerifierEnabled ||
		subtaskID == nil || fp.cfg.SmallModelSubtaskMaxCommands <= 0 {
		return 0, false
	}
	n := fp.smallModelState().SubtaskCommands(*subtaskID)
	return n, n >= fp.cfg.SmallModelSubtaskMaxCommands
}

// budgetSpentTools are withheld once a subtask's command budget is spent: each
// runs commands, directly or through the agent it delegates to.
var budgetSpentTools = map[string]struct{}{
	tools.TerminalToolName:    {},
	tools.PentesterToolName:   {},
	tools.CoderToolName:       {},
	tools.MaintenanceToolName: {},
}

// budgetSpentExecutor is the executor an agent works with once its subtask's
// command budget is spent. A small model ignores "stop" written in a tool answer
// but cannot call a tool it is not offered, so the tools that run commands are
// left out of the schemas it is sent. A call that still names one, copied from
// earlier in the chain, is answered with the budget message instead of running.
type budgetSpentExecutor struct {
	tools.ContextToolsExecutor

	message string
}

func (e budgetSpentExecutor) Tools() []llms.Tool {
	all := e.ContextToolsExecutor.Tools()
	kept := make([]llms.Tool, 0, len(all))
	for _, tool := range all {
		if tool.Function != nil {
			if _, withheld := budgetSpentTools[tool.Function.Name]; withheld {
				continue
			}
		}
		kept = append(kept, tool)
	}
	return kept
}

func (e budgetSpentExecutor) IsFunctionExists(name string) bool {
	if _, withheld := budgetSpentTools[name]; withheld {
		return false
	}
	return e.ContextToolsExecutor.IsFunctionExists(name)
}

func (e budgetSpentExecutor) Execute(
	ctx context.Context, streamID int64, id, name, obsName, thinking string, args json.RawMessage,
) (string, error) {
	if _, withheld := budgetSpentTools[name]; withheld {
		logrus.WithContext(ctx).WithField("func_name", name).Warn("small-model guard refused a tool withheld by the spent subtask budget")
		return e.message, nil
	}
	return e.ContextToolsExecutor.Execute(ctx, streamID, id, name, obsName, thinking, args)
}

// smallModelExecutorFor returns the executor an agent of the subtask works with
// on its next turn: the one it was given, or, once the subtask's command budget is
// spent, a view of it that withholds the tools that run commands.
func (fp *flowProvider) smallModelExecutorFor(
	executor tools.ContextToolsExecutor, subtaskID *int64,
) tools.ContextToolsExecutor {
	n, spent := fp.smallModelSubtaskBudgetSpent(subtaskID)
	if !spent {
		return executor
	}
	return budgetSpentExecutor{
		ContextToolsExecutor: executor,
		message: smallmodel.SubtaskBudgetSpentMessage(
			n, fp.cfg.SmallModelSubtaskMaxCommands, executor.GetBarrierToolNames(),
		),
	}
}

// failureRecordedTools are the tools whose identical calls are answered from the
// failure record: their answer is a function of the call and of the sandbox or
// page it reads, not of time, as a web search's is.
var failureRecordedTools = map[string]struct{}{
	tools.TerminalToolName: {},
	tools.FileToolName:     {},
	tools.BrowserToolName:  {},
}

// smallModelCallKey is the form in which two calls of a subtask count as the
// same call for the failure record, or false for a call the record ignores.
func (fp *flowProvider) smallModelCallKey(call *llms.FunctionCall, subtaskID *int64) (string, bool) {
	if !fp.smallModelEnabled() || subtaskID == nil || call == nil {
		return "", false
	}
	if _, ok := failureRecordedTools[call.Name]; !ok {
		return "", false
	}
	cleared := clearCallArguments(call)
	return cleared.Name + "\n" + cleared.Arguments, true
}

// smallModelRepeatedFailure returns the answer for a call that already failed
// twice in the subtask with the same result, so it is not run a third time.
func (fp *flowProvider) smallModelRepeatedFailure(call *llms.FunctionCall, subtaskID *int64) (string, bool) {
	key, ok := fp.smallModelCallKey(call, subtaskID)
	if !ok {
		return "", false
	}
	answer, repeated := fp.smallModelState().RepeatedFailure(*subtaskID, key)
	if !repeated {
		return "", false
	}
	logrus.WithFields(logrus.Fields{
		"flow": fp.flowID, "subtask": *subtaskID, "guard": "repeated_failure", "func_name": call.Name,
	}).Warn("small-model guard answered a repeatedly failing call from the record")
	return smallmodel.RepeatedFailureMessage(answer, repeatEchoLimit), true
}

// smallModelRecordCallResult files the raw answer a call got, for the failure
// record. It runs on the answer before compaction, which can cut the marker that
// shows the failure.
func (fp *flowProvider) smallModelRecordCallResult(call *llms.FunctionCall, response string, subtaskID *int64) {
	key, ok := fp.smallModelCallKey(call, subtaskID)
	if !ok {
		return
	}
	fp.smallModelState().RecordCallResult(*subtaskID, key, response, smallmodel.LooksFailed(response))
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

// recordLoopCommand files the canonical form of a terminal command or a file call
// into the per-task loop-detection ring before it executes, so
// DetectStuckByRepetition can see a repeated gesture on the next turn. Only a
// terminal command is charged to the subtask's command budget: re-reading the
// same saved output is a loop worth seeing, but it runs nothing.
func (fp *flowProvider) recordLoopCommand(funcName string, funcArgs json.RawMessage, taskID, subtaskID *int64) {
	if !fp.smallModelEnabled() || taskID == nil {
		return
	}
	key := smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID}

	switch funcName {
	case tools.TerminalToolName:
		var action struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(funcArgs, &action); err != nil || action.Input == "" {
			return
		}
		fp.smallModelState().RecordCommand(key, smallmodel.CanonicalizeCommand(action.Input))
		if subtaskID != nil {
			fp.smallModelState().IncrSubtaskCommands(*subtaskID)
		}
	case tools.FileToolName:
		var action struct {
			Action string `json:"action"`
			Path   string `json:"path"`
		}
		if err := json.Unmarshal(funcArgs, &action); err != nil || action.Path == "" {
			return
		}
		fp.smallModelState().RecordCommand(key, "file "+action.Action+" "+smallmodel.CanonicalizeCommand(action.Path))
	}
}

// logSmallModelState writes the loop-detection state of the task the agent is
// working on, once per turn. The state lives only in memory and is not part of
// the stored chain, so this line is the only record of why a recovery directive
// did or did not fire.
func (fp *flowProvider) logSmallModelState(logger *logrus.Entry, taskID, subtaskID *int64) {
	if !fp.smallModelEnabled() || taskID == nil {
		return
	}
	snap := fp.smallModelState().TakeSnapshot(smallmodel.Key{FlowID: fp.flowID, TaskID: *taskID})
	fields := logrus.Fields{
		"facts":               snap.Facts,
		"new_facts":           snap.NewFacts,
		"failed_tries":        snap.FailedTries,
		"recent_commands":     snap.RecentCommands,
		"cmds_since_progress": snap.CmdsSinceProgress,
		"blocked_gestures":    snap.BlockedGestures,
	}
	if subtaskID != nil {
		fields["subtask_commands"] = fp.smallModelState().SubtaskCommands(*subtaskID)
		fields["subtask_command_budget"] = fp.cfg.SmallModelSubtaskMaxCommands
	}
	logger.WithFields(fields).Info("small-model state")
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

const (
	// smallModelCompactionAttempts is how many summarizer failures a compaction
	// tolerates before it drops old steps without the model.
	smallModelCompactionAttempts   = 3
	smallModelCompactionRetryDelay = time.Second
	// smallModelCompactionLevels is how many ever tighter passes a compaction
	// makes while the summarized chain is still over budget.
	smallModelCompactionLevels = 3
)

// smallModelDroppedSteps stands in for the summary of old steps when the
// summarizer stays down; the state block keeps the facts those steps produced.
const smallModelDroppedSteps = "Earlier steps were removed to fit the context window because the " +
	"summarizer was unavailable. The CURRENT STATE block carries the facts they produced."

// smallModelCompactIfOverBudget compacts the chain of role opt when it exceeds
// that role's token budget. The summarizer is sized to the budget rather than to
// the global SUMMARIZER_* thresholds, which know nothing of the role's window and
// can leave a compacted chain still too large for it.
//
// A chain known to be over budget is not sent as is. A pass that leaves it over
// budget is followed by a tighter one; a failing summarizer is retried with
// backoff, and when it stays down the old steps are dropped without the model.
// Only a cancelled context returns the chain unchanged.
func (fp *flowProvider) smallModelCompactIfOverBudget(
	ctx context.Context,
	opt pconfig.ProviderOptionsType,
	handler tools.SummarizeHandler,
	chain []llms.MessageContent,
	schemas []llms.Tool,
) ([]llms.MessageContent, bool) {
	if !fp.smallModelEnabled() || fp.summarizer == nil {
		return chain, false
	}
	budget := fp.smallModelBudget(opt, schemas)
	if !budget.Exceeded(chain) {
		return chain, false
	}

	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"agent":        string(opt),
		"token_limit":  budget.TokenLimit(),
		"chain_tokens": budget.Tokens(chain),
	})
	current, changed := chain, false
	for level, failures := 1, 0; level <= smallModelCompactionLevels; {
		compacted, err := smallModelBudgetSummarizer(budget, level).SummarizeChain(ctx, handler, current, fp.tcIDTemplate)
		if err != nil {
			if ctx.Err() != nil {
				return chain, false
			}
			failures++
			logger.WithError(err).WithField("attempt", failures).Warn("small-model compaction failed")
			if failures == smallModelCompactionAttempts {
				break
			}
			if !smallModelWait(ctx, smallModelCompactionRetryDelay*time.Duration(failures)) {
				return chain, false
			}
			continue
		}

		current, changed = compacted, true
		if !budget.Exceeded(current) {
			return current, true
		}
		level++
	}

	dropOldSteps := func(context.Context, string) (string, error) { return smallModelDroppedSteps, nil }
	dropped, err := smallModelBudgetSummarizer(budget, smallModelCompactionLevels).
		SummarizeChain(ctx, dropOldSteps, current, fp.tcIDTemplate)
	if err != nil {
		if changed {
			return current, true
		}
		logger.WithError(err).Error("small-model compaction failed, sending the chain uncompacted")
		return chain, false
	}
	if budget.Exceeded(dropped) {
		logger.Warn("chain is still over budget after compaction; the provider lowers the answer to the room left")
	}
	return dropped, true
}

// smallModelBudgetSummarizer sizes a summarizer to the budget, a level-th of it at
// level > 1. The first level keeps the human messages of summarized sections
// verbatim; that can leave the chain over budget when they are large (a long
// task scope), so the tighter levels summarize them too.
func smallModelBudgetSummarizer(budget smallmodel.Budget, level int) csum.Summarizer {
	bytesPerToken := budget.BytesPerToken
	if bytesPerToken <= 0 {
		bytesPerToken = 3
	}
	target := budget.TokenLimit() * bytesPerToken / max(level, 1)

	return csum.NewSummarizer(csum.SummarizerConfig{
		PreserveLast:   true,
		UseQA:          true,
		SummHumanInQA:  level > 1,
		LastSecBytes:   target / 2,
		MaxBPBytes:     target / 4,
		MaxQABytes:     target,
		KeepQASections: 1,
	})
}

func smallModelWait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
