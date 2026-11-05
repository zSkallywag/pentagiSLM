package smallmodel

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vxcontrol/langchaingo/llms"
)

func msg(text string) llms.MessageContent {
	return llms.MessageContent{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextContent{Text: text}},
	}
}

func TestBudget_TokenLimit_IsWindowTimesPercent(t *testing.T) {
	b := Budget{CtxWindow: 32000, Percent: 40}
	assert.Equal(t, 12800, b.TokenLimit())
}

func TestBudget_TokenLimit_LeavesTheReserveFree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		window  int
		percent int
		reserve int
		want    int
	}{
		{name: "no reserve keeps the share of the window", window: 16384, percent: 40, reserve: 0, want: 6553},
		{name: "a reserve that leaves more than the share changes nothing", window: 16384, percent: 40, reserve: 8792, want: 6553},
		{name: "a reserve that leaves less than the share lowers the limit", window: 16384, percent: 60, reserve: 8192, want: 8192},
		{name: "answer and tools together lower the limit", window: 8192, percent: 80, reserve: 4000, want: 4192},
		{name: "a reserve equal to the window is ignored", window: 16384, percent: 40, reserve: 16384, want: 6553},
		{name: "a reserve above the window is ignored", window: 16384, percent: 40, reserve: 32768, want: 6553},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Budget{CtxWindow: tc.window, Percent: tc.percent, ReserveTokens: tc.reserve}
			assert.Equal(t, tc.want, b.TokenLimit())
		})
	}
}

func TestBudget_Exceeded_CompactsEarlierWhenTheAnswerNeedsRoom(t *testing.T) {
	b := Budget{CtxWindow: 1000, Percent: 80, BytesPerToken: 3, ReserveTokens: 500}
	chain := []llms.MessageContent{msg(strings.Repeat("a", 1800))} // 600 tokens

	assert.False(t, Budget{CtxWindow: 1000, Percent: 80, BytesPerToken: 3}.Exceeded(chain))
	assert.True(t, b.Exceeded(chain))
}

func TestBudget_EstimateToolTokens_CountsTheSchemas(t *testing.T) {
	assert.Equal(t, 0, EstimateToolTokens(nil, 3))

	tools := []llms.Tool{{
		Type:     "function",
		Function: &llms.FunctionDefinition{Name: "terminal", Description: strings.Repeat("d", 3000)},
	}}
	got := EstimateToolTokens(tools, 3)
	assert.GreaterOrEqual(t, got, 1000)
	assert.Less(t, got, 1100)
}

func TestBudget_Exceeded_UsesByteEstimateWhenNoTokenizer(t *testing.T) {
	b := Budget{CtxWindow: 1000, Percent: 40, BytesPerToken: 3}
	limitBytes := b.TokenLimit() * b.BytesPerToken // 400 tokens -> 1200 bytes

	under := []llms.MessageContent{msg(strings.Repeat("a", limitBytes-30))}
	over := []llms.MessageContent{msg(strings.Repeat("a", limitBytes+30))}

	assert.False(t, b.Exceeded(under))
	assert.True(t, b.Exceeded(over))
}

func TestBudget_Tokens_PrefersTokenizerOverEstimate(t *testing.T) {
	b := Budget{
		CtxWindow:     1000,
		Percent:       40,
		BytesPerToken: 3,
		CountTokens: func([]llms.MessageContent) (int, error) {
			return 7, nil
		},
	}
	assert.Equal(t, 7, b.Tokens([]llms.MessageContent{msg(strings.Repeat("a", 9000))}))
}

func TestBudget_Tokens_FallsBackWhenTokenizerErrors(t *testing.T) {
	b := Budget{
		CtxWindow:     1000,
		Percent:       40,
		BytesPerToken: 3,
		CountTokens: func([]llms.MessageContent) (int, error) {
			return 0, errors.New("tokenizer unavailable")
		},
	}
	assert.Equal(t, 300, b.Tokens([]llms.MessageContent{msg(strings.Repeat("a", 900))}))
}

func TestBudget_Exceeded_DisabledWhenWindowNonPositive(t *testing.T) {
	b := Budget{CtxWindow: 0, Percent: 40, BytesPerToken: 3}
	assert.False(t, b.Exceeded([]llms.MessageContent{msg(strings.Repeat("a", 1_000_000))}))
}
