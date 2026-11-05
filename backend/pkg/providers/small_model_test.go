package providers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"pentagi/pkg/csum"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"
	"pentagi/pkg/providers/tester/mock"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vxcontrol/langchaingo/llms"
)

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
		failing       bool
		wantDone      bool
		wantSummaries bool
	}{
		{
			name: "a role on a 16K model compacts where the global window would not", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, wantDone: true, wantSummaries: true,
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
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, wantDone: true, wantSummaries: true,
		},
		{
			name: "the answer's room lowers the limit below the share of the window", smallModel: true,
			globalWindow: 32768, percent: 80, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 10000\n", turns: 7, wantDone: true, wantSummaries: true,
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
			tools: windowCompactionTools(20000), wantDone: true, wantSummaries: true,
		},
		{
			name: "nothing is compacted without small-model mode", smallModel: false,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8,
		},
		{
			name: "a failed summarisation keeps the chain", smallModel: true,
			globalWindow: 32768, percent: 40, modelWindow: window(16384),
			roleConfig: "pentester:\n  max_tokens: 8192\n", turns: 8, failing: true, wantSummaries: true,
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
			handler := func(context.Context, string) (string, error) {
				calls.Add(1)
				if tt.failing {
					return "", errors.New("summarizer is down")
				}
				return "short summary", nil
			}

			chain := windowCompactionChain(tt.turns, 1500)
			compacted, done := fp.smallModelCompactIfOverBudget(
				context.Background(), pconfig.OptionsTypePentester, handler, chain, tt.tools,
			)

			assert.Equal(t, tt.wantDone, done)
			if tt.wantDone {
				assert.Less(t, windowCompactionBytes(compacted), windowCompactionBytes(chain))
			} else {
				assert.Equal(t, windowCompactionChain(tt.turns, 1500), compacted)
			}
			assert.Equal(t, tt.wantSummaries, calls.Load() > 0, "summarisation calls made: %d", calls.Load())
		})
	}
}
