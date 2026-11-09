package providers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"pentagi/pkg/csum"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/providers/tester/mock"
	"pentagi/pkg/smallmodel"
	"pentagi/pkg/tools"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vxcontrol/langchaingo/llms"
)

func smallModelChainMentions(chain []llms.MessageContent, text string) bool {
	for _, message := range chain {
		for _, part := range message.Parts {
			switch p := part.(type) {
			case llms.TextContent:
				if strings.Contains(p.Text, text) {
					return true
				}
			case llms.ToolCallResponse:
				if strings.Contains(p.Content, text) {
					return true
				}
			}
		}
	}
	return false
}

func TestSmallModel_SmallModelCompactIfOverBudget_MeasuresTheChainAgainstItsRolesWindow(t *testing.T) {
	t.Parallel()

	window := func(v int) *int { return &v }

	// An 8-turn chain of 1500-byte messages is about 8000 tokens at 3 bytes per token.
	tests := []struct {
		name          string
		smallModel    bool
		globalWindow  int
		percent       int
		modelWindow   *int
		roleConfig    string
		turns         int
		tools         []llms.Tool
		failures      int // summarizer calls that fail before it answers; -1 fails every call
		cancelled     bool
		wantDone      bool
		wantSummaries bool
		wantDropped   bool
		wantFits      int // the role's token limit a compacted chain must fit
	}{
		{
			name: "a role on a 16K model compacts where the global window would not", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, wantDone: true, wantSummaries: true, wantFits: 6553,
		},
		{
			name: "a role on a large cloud model keeps its context", smallModel: true,
			globalWindow: 16384, percent: 40, modelWindow: window(131072),
			roleConfig: "pentester:\n  max_tokens: 16384\n", turns: 8,
		},
		{
			name: "an unknown window falls back to the global one", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: nil,
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8,
		},
		{
			name: "a small global window still applies to a model with an unknown window", smallModel: true,
			globalWindow: 16384, percent: 40, modelWindow: nil,
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, wantDone: true, wantSummaries: true, wantFits: 6553,
		},
		{
			name: "the answer's room lowers the limit below the share of the window", smallModel: true,
			globalWindow: 32768, percent: 80, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 10000\n", turns: 7, wantDone: true, wantSummaries: true, wantFits: 6384,
		},
		{
			name: "a chain that fits beside the answer is left alone", smallModel: true,
			globalWindow: 32768, percent: 80, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 4000\n", turns: 7,
		},
		{
			name: "the tool schemas take room from the chain", smallModel: true,
			globalWindow: 32768, percent: 80, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 4000\n", turns: 7,
			tools: windowCompactionTools(20000), wantDone: true, wantSummaries: true, wantFits: 5684,
		},
		{
			name: "nothing is compacted without small-model mode", smallModel: false,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8,
		},
		{
			name: "a summarizer that fails once is asked again", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, failures: 1, wantDone: true, wantSummaries: true, wantFits: 6553,
		},
		{
			name: "a summarizer that stays down leads to dropping old steps", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, failures: -1,
			wantDone: true, wantSummaries: true, wantDropped: true, wantFits: 6553,
		},
		{
			name: "a cancelled context sends the chain as is", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, cancelled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			providerConfig, err := pconfig.LoadConfigData([]byte(tt.roleConfig), nil)
			require.NoError(t, err)
			prv := mock.NewProvider(provider.ProviderOllama, "ollama", "role-model")
			prv.SetModels(pconfig.ModelsConfig{{Name: "role-model", ContextWindow: tt.modelWindow}})
			prv.SetProviderConfig(providerConfig)

			fp := newFlowProvider()
			fp.cfg.SmallModelMode = tt.smallModel
			fp.cfg.SmallModelCtxWindow = tt.globalWindow
			fp.cfg.SmallModelCtxBudgetPercent = tt.percent
			fp.cfg.SmallModelBytesPerToken = 3
			fp.summarizer = csum.NewSummarizer(csum.SummarizerConfig{})
			fp.tcIDTemplate = "call_%d"
			fp.Provider = prv

			var calls atomic.Int64
			handler := func(ctx context.Context, _ string) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				n := calls.Add(1)
				if tt.failures < 0 || n <= int64(tt.failures) {
					return "", errors.New("summarizer is down")
				}
				return "short summary", nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			chain := windowCompactionChain(tt.turns, 1500)
			compacted, done := fp.smallModelCompactIfOverBudget(
				ctx, pconfig.OptionsTypePentester, handler, chain, tt.tools,
			)

			assert.Equal(t, tt.wantDone, done)
			if tt.wantDone {
				assert.Less(t, windowCompactionBytes(compacted), windowCompactionBytes(chain))
				assert.LessOrEqual(t, windowCompactionBytes(compacted)/3, tt.wantFits,
					"the compacted chain must fit the role's limit, not just be smaller")
			} else {
				assert.Equal(t, windowCompactionChain(tt.turns, 1500), compacted)
			}
			assert.Equal(t, tt.wantSummaries, calls.Load() > 0, "summarisation calls made: %d", calls.Load())
			if tt.failures < 0 {
				assert.GreaterOrEqual(t, calls.Load(), int64(3), "the summarizer is asked once per attempt")
			}
			assert.Equal(t, tt.wantDropped, smallModelChainMentions(compacted, "summarizer was unavailable"))
		})
	}
}

// smallModelCountingExecutor exists for every name and counts the calls that reach it.
type smallModelCountingExecutor struct {
	tools.ContextToolsExecutor

	executed []string
}

func (e *smallModelCountingExecutor) Execute(
	_ context.Context, _ int64, _, name, _, _ string, _ json.RawMessage,
) (string, error) {
	e.executed = append(e.executed, name)
	return "ran " + name, nil
}

func (*smallModelCountingExecutor) IsFunctionExists(string) bool { return true }

func (*smallModelCountingExecutor) GetBarrierToolNames() []string {
	return []string{tools.FinalyToolName}
}

func TestSmallModel_SmallModelExecutorFor_WithholdsTheCommandToolsOnceTheBudgetIsSpent(t *testing.T) {
	fp := newFlowProvider()
	fp.cfg.SmallModelMode = true
	fp.cfg.SmallModelVerifierEnabled = true
	fp.cfg.SmallModelSubtaskMaxCommands = 2
	subtaskID := int64(152)
	inner := &smallModelCountingExecutor{}

	fp.smallModelState().IncrSubtaskCommands(subtaskID)
	assert.Same(t, inner, fp.smallModelExecutorFor(inner, &subtaskID), "within the budget the agent keeps its executor")
	assert.Same(t, inner, fp.smallModelExecutorFor(inner, nil), "an agent outside a subtask has no budget")

	fp.smallModelState().IncrSubtaskCommands(subtaskID)
	spent := fp.smallModelExecutorFor(inner, &subtaskID)

	for _, name := range []string{tools.TerminalToolName, tools.PentesterToolName, tools.CoderToolName, tools.MaintenanceToolName} {
		assert.False(t, spent.IsFunctionExists(name), "%s must be withheld", name)
		answer, err := spent.Execute(context.Background(), 0, "call_1", name, name, "", json.RawMessage(`{}`))
		require.NoError(t, err)
		assert.Contains(t, answer, "SUBTASK BUDGET EXHAUSTED: this subtask has run 2 terminal commands (limit 2)")
		assert.Contains(t, answer, "call done", "the answer must name the way to finish")
	}
	assert.Empty(t, inner.executed, "a withheld tool must not run")

	assert.True(t, spent.IsFunctionExists(tools.FileToolName))
	answer, err := spent.Execute(context.Background(), 0, "call_2", tools.FileToolName, tools.FileToolName, "", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "ran file", answer)
	assert.Equal(t, []string{tools.FileToolName}, inner.executed)
}

func TestSmallModel_RecordLoopCommand_RingsFileCallsWithoutChargingTheBudget(t *testing.T) {
	fp := newFlowProvider()
	fp.cfg.SmallModelMode = true
	taskID, subtaskID := int64(9), int64(152)

	fp.recordLoopCommand(tools.TerminalToolName, json.RawMessage(`{"input":"id","message":"who am I"}`), &taskID, &subtaskID)
	fp.recordLoopCommand(tools.FileToolName,
		json.RawMessage(`{"action":"read_file","path":"/work/transform_resp_6761229_v3.json","message":"reading the saved response"}`),
		&taskID, &subtaskID)
	fp.recordLoopCommand(tools.FileToolName, json.RawMessage(`{"action":"read_file","message":"no path"}`), &taskID, &subtaskID)

	recent, since := fp.smallModelState().RepetitionState(smallmodel.Key{TaskID: taskID})
	assert.Equal(t, []string{"id", "file read_file /work/*.json"}, recent)
	assert.Equal(t, 2, since, "a file call counts as a command since the last progress")
	assert.Equal(t, 1, fp.smallModelState().SubtaskCommands(subtaskID), "only the terminal command is charged to the budget")
}

func TestSmallModel_LogSmallModelState_WritesTheLoopStateOfEachTurn(t *testing.T) {
	hook := logtest.NewGlobal()
	taskID, subtaskID := int64(9), int64(152)

	fp := newFlowProvider()
	fp.cfg.SmallModelMode = true
	fp.cfg.SmallModelSubtaskMaxCommands = 150
	key := smallmodel.Key{TaskID: taskID}
	fp.smallModelState().AddFacts(key, "host seen: 3.161.82.3")
	fp.recordLoopCommand(tools.TerminalToolName, json.RawMessage(`{"input":"id"}`), &taskID, &subtaskID)

	stateEntries := func() []*logrus.Entry {
		var found []*logrus.Entry
		for _, entry := range hook.AllEntries() {
			if entry.Message == "small-model state" {
				found = append(found, entry)
			}
		}
		return found
	}

	fp.logSmallModelState(logrus.NewEntry(logrus.StandardLogger()), &taskID, &subtaskID)
	fp.logSmallModelState(logrus.NewEntry(logrus.StandardLogger()), &taskID, &subtaskID)

	entries := stateEntries()
	require.Len(t, entries, 2)
	assert.Equal(t, logrus.InfoLevel, entries[0].Level)
	assert.Equal(t, 1, entries[0].Data["facts"])
	assert.Equal(t, []string{"host seen: 3.161.82.3"}, entries[0].Data["new_facts"])
	assert.Equal(t, []string{"id"}, entries[0].Data["recent_commands"])
	assert.Equal(t, 1, entries[0].Data["cmds_since_progress"])
	assert.Equal(t, 1, entries[0].Data["subtask_commands"])
	assert.Equal(t, 150, entries[0].Data["subtask_command_budget"])
	assert.Empty(t, entries[1].Data["new_facts"], "a fact is reported once, on the turn it appeared")

	hook.Reset()
	fp.cfg.SmallModelMode = false
	fp.logSmallModelState(logrus.NewEntry(logrus.StandardLogger()), &taskID, &subtaskID)
	assert.Empty(t, stateEntries(), "without the small-model profile there is no state to log")
}
