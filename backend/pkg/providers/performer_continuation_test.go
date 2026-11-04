package providers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"pentagi/pkg/database"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/templates"
	"pentagi/pkg/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/llms/streaming"
)

// continuationScriptItem represents one entry in a call script.
type continuationScriptItem struct {
	content    string
	stopReason string
	toolCalls  []llms.ToolCall
	callErr    error
}

// continuationFakeProvider scripts a sequence of responses per slot index.
// Each CallWithTools response is returned in order; once the script is
// exhausted the last entry is repeated. CallEx (used by reflector's
// performSimpleChain) returns callExResp if non-nil, otherwise callExErr.
type continuationFakeProvider struct {
	provider.Provider

	mu           sync.Mutex
	script       []continuationScriptItem
	calls        int // number of CallWithTools invocations
	chains       [][]llms.MessageContent // CallWithTools chains only
	callExChains [][]llms.MessageContent // CallEx chains only
	callExResp   *llms.ContentResponse   // returned by CallEx when set
	callExErr    error                   // returned by CallEx when resp is nil
}

func (f *continuationFakeProvider) Type() provider.ProviderType { return provider.ProviderOpenAI }

func (f *continuationFakeProvider) GetUsage(map[string]any) pconfig.CallUsage { return pconfig.CallUsage{} }

func (f *continuationFakeProvider) GetPriceInfo(pconfig.ProviderOptionsType) *pconfig.PriceInfo { return nil }

func (f *continuationFakeProvider) CallEx(
	_ context.Context,
	_ pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	_ streaming.Callback,
) (*llms.ContentResponse, error) {
	f.mu.Lock()
	f.callExChains = append(f.callExChains, chain)
	f.mu.Unlock()
	if f.callExResp != nil {
		return f.callExResp, nil
	}
	if f.callExErr != nil {
		return nil, f.callExErr
	}
	// By default, CallEx errors so the reflector path terminates quickly.
	return nil, errors.New("CallEx not supported in continuation test")
}

func (f *continuationFakeProvider) CallWithTools(
	_ context.Context,
	_ pconfig.ProviderOptionsType,
	chain []llms.MessageContent,
	_ []llms.Tool,
	_ streaming.Callback,
) (*llms.ContentResponse, error) {
	f.mu.Lock()
	f.calls++
	f.chains = append(f.chains, chain)
	idx := f.calls - 1
	f.mu.Unlock()

	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	item := f.script[idx]
	if item.callErr != nil {
		return nil, item.callErr
	}

	if item.content == "" && len(item.toolCalls) == 0 && item.stopReason == "" {
		return &llms.ContentResponse{Choices: []*llms.ContentChoice{}}, nil
	}

	choices := []*llms.ContentChoice{{
		Content:    item.content,
		StopReason: item.stopReason,
		ToolCalls:  item.toolCalls,
	}}
	return &llms.ContentResponse{Choices: choices}, nil
}

// oneToolExec is a minimal executor that provides one tool.
type oneToolExec struct {
	tools.ContextToolsExecutor
	toolName string
}

func (e oneToolExec) Tools() []llms.Tool                   { return []llms.Tool{{Function: &llms.FunctionDefinition{Name: e.toolName}}} }
func (e oneToolExec) GetBarrierToolNames() []string        { return []string{e.toolName} }
func (e oneToolExec) GetBarrierTools() []tools.FunctionInfo { return nil }

// chainTextContents returns all text content strings in a chain.
func chainTextContents(chain []llms.MessageContent) []string {
	var texts []string
	for _, msg := range chain {
		for _, part := range msg.Parts {
			if tc, ok := part.(llms.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
	}
	return texts
}

// continuationDirective is the human message sent to trigger a continuation.
const continuationDirective = "Continue your previous answer from where it stopped. Do not repeat content you have already provided."

// newToolCalls returns a tool call for the "report" tool.
func newToolCalls() []llms.ToolCall {
	return []llms.ToolCall{{
		ID:           "call_1",
		Type:         "function",
		FunctionCall: &llms.FunctionCall{Name: "report", Arguments: `{}`},
	}}
}

// continuationDbQuerier satisfies database.Querier for the reflector path.
// It embeds the full interface and overrides only the methods actually called.
type continuationDbQuerier struct {
	database.Querier
}

func (continuationDbQuerier) CreateMsgChain(context.Context, database.CreateMsgChainParams) (database.Msgchain, error) {
	return database.Msgchain{}, nil
}

func (continuationDbQuerier) UpdateMsgChainUsage(context.Context, database.UpdateMsgChainUsageParams) (database.Msgchain, error) {
	return database.Msgchain{}, nil
}

// TestPerformer_Continuation_Suite covers the T1 continuation loop cases.
func TestPerformer_Continuation_Suite(t *testing.T) {
	baseChain := []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "write the report")}

	// -------------------------------------------------------------------------
	// Case 1: Mode ON, budget 2 — 'length'/empty triggers continuation, second
	// call returns tool calls → success. Continuation directive must appear in
	// the second call's chain.
	// -------------------------------------------------------------------------
	t.Run("case1_modeON_budget2_truncation_empty_then_toolCall", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "here is the report", stopReason: "stop", toolCalls: newToolCalls()})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 2
		fp.cfg.SmallModelMode = true
		fp.Provider = prv

		result, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.NotEmpty(t, result.funcCalls, "second response had tool calls")
		assert.Equal(t, "report", result.funcCalls[0].FunctionCall.Name)

		prv.mu.Lock()
		calls := prv.calls
		chain2 := prv.chains[1]
		prv.mu.Unlock()
		assert.Equal(t, 2, calls, "first call triggered continuation, second succeeded")

		texts2 := chainTextContents(chain2)
		foundDirective := false
		for _, txt := range texts2 {
			if strings.Contains(txt, continuationDirective) {
				foundDirective = true
				break
			}
		}
		assert.True(t, foundDirective, "second call must carry the continuation directive")
	})

	// -------------------------------------------------------------------------
	// Case 2: Mode ON — 'length' with partial content. Accumulated pieces are
	// joined in final content. Second call chain must include the partial text
	// AND the continuation directive.
	// -------------------------------------------------------------------------
	t.Run("case2_modeON_partialContent_bothPiecesJoined", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "first half of the answer", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "stop", toolCalls: newToolCalls()})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 2
		fp.cfg.SmallModelMode = true
		fp.Provider = prv

		result, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Contains(t, result.content, "first half of the answer",
			"partial content from first attempt must be accumulated")

		prv.mu.Lock()
		chain2 := prv.chains[1]
		prv.mu.Unlock()
		texts2 := chainTextContents(chain2)
		foundPartial := false
		foundDirective := false
		for _, txt := range texts2 {
			if strings.Contains(txt, "first half of the answer") {
				foundPartial = true
			}
			if strings.Contains(txt, continuationDirective) {
				foundDirective = true
			}
		}
		assert.True(t, foundPartial, "second call chain must include accumulated partial content")
		assert.True(t, foundDirective, "second call chain must include continuation directive")
	})

	// -------------------------------------------------------------------------
	// Case 3: Budget exhaustion — all responses 'length'/empty. After
	// SmallModelMaxContinuations=2, loop must not infinite-loop; final error
	// must mention max retries and prior attempts.
	// -------------------------------------------------------------------------
	t.Run("case3_budgetExhaustion_maxRetriesError", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 2
		fp.cfg.SmallModelMode = true
		fp.Provider = prv
		fp.db = continuationDbQuerier{}
		fp.prompter = templates.NewDefaultPrompter()
		// Make CallEx (used by reflector's performSimpleChain) return a valid
		// prose response so the reflector doesn't recurse — the reflector's own
		// CallWithTools will then get the script's 3rd entry (length truncation).
		prv.callExErr = nil // ensure CallEx succeeds

		_, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.Error(t, err, "must error after exhausting all retries")
		assert.Contains(t, err.Error(), "max retries reached",
			"error must mention max retries")
		assert.Contains(t, err.Error(), "prior attempts",
			"error must reference prior attempts")
	})

	// -------------------------------------------------------------------------
	// Case 4: Mode OFF — 'length'/empty must NOT emit continuation messages.
	// All calls must use the original baseChain (no appended AI/human msgs).
	// -------------------------------------------------------------------------
	t.Run("case4_modeOFF_noContinuationMessages", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 2
		fp.cfg.SmallModelMode = false
		fp.Provider = prv
		fp.db = continuationDbQuerier{}
		fp.prompter = templates.NewDefaultPrompter()

		_, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.Error(t, err)
		prv.mu.Lock()
		defer prv.mu.Unlock()
		// T1 invariant: outer retries (calls 0-2) must use unmodified baseChain.
		for i := 0; i < 3; i++ {
			assert.Len(t, prv.chains[i], 1, "outer retry call %d must use the unmodified original chain", i)
		}
		// T1 invariant: continuation directive must never appear in any chain.
		for _, chain := range prv.chains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
		for _, chain := range prv.callExChains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
	})

	// -------------------------------------------------------------------------
	// Case 5: Budget 0 with mode ON must behave identically to mode OFF.
	// -------------------------------------------------------------------------
	t.Run("case5_budget0_modeON_sameAsOff", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})
		script = append(script, continuationScriptItem{content: "", stopReason: "length"})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 0
		fp.cfg.SmallModelMode = true
		fp.Provider = prv
		fp.db = continuationDbQuerier{}
		fp.prompter = templates.NewDefaultPrompter()

		_, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.Error(t, err)
		prv.mu.Lock()
		defer prv.mu.Unlock()
		// T1 invariant: outer retries (calls 0-2) must use unmodified baseChain.
		for i := 0; i < 3; i++ {
			assert.Len(t, prv.chains[i], 1, "outer retry call %d must use the unmodified original chain", i)
		}
		// T1 invariant: continuation directive must never appear in any chain.
		for _, chain := range prv.chains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
		for _, chain := range prv.callExChains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
	})

	// -------------------------------------------------------------------------
	// Case 6: Empty response with stop reason 'stop' (not 'length') — no
	// continuation. Normal error path: error appended to errs, loop retries.
	// No continuation messages ever added.
	// -------------------------------------------------------------------------
	t.Run("case6_emptyResponse_stopReasonStop_normalErrorPath", func(t *testing.T) {
		var script []continuationScriptItem
		script = append(script, continuationScriptItem{content: "", stopReason: "stop"})
		script = append(script, continuationScriptItem{content: "", stopReason: "stop"})
		script = append(script, continuationScriptItem{content: "", stopReason: "stop"})

		prv := &continuationFakeProvider{script: script}
		fp := newFlowProvider()
		fp.cfg.SmallModelMaxContinuations = 2
		fp.cfg.SmallModelMode = true
		fp.Provider = prv
		fp.db = continuationDbQuerier{}
		fp.prompter = templates.NewDefaultPrompter()

		_, err := fp.callWithRetries(
			context.Background(),
			pconfig.OptionsTypeCoder,
			1, nil, nil,
			baseChain,
			oneToolExec{toolName: "report"},
			"run",
		)

		require.Error(t, err)
		prv.mu.Lock()
		defer prv.mu.Unlock()
		assert.Equal(t, 6, prv.calls, "3 outer retries + 3 reflector retries")
		// T1 invariant: outer retries (calls 0-2) must use unmodified baseChain.
		for i := 0; i < 3; i++ {
			assert.Len(t, prv.chains[i], 1,
				"outer retry call %d must use the unmodified original chain", i)
		}
		// T1 invariant: continuation directive must never appear in any chain.
		for _, chain := range prv.chains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
		for _, chain := range prv.callExChains {
			texts := chainTextContents(chain)
			for _, txt := range texts {
				assert.False(t, strings.Contains(txt, continuationDirective),
					"continuation directive must never be sent")
			}
		}
	})
}
