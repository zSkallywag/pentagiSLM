// Package smallmodel holds the helpers that make PentAGI usable with small
// local models: a token budget enforced before each model call, a structured
// pentest-state block that survives summarization, deterministic extraction of
// facts from tool output, pre-processing of raw tool output, and a
// pre-execution verifier that keeps the agent inside the authorized scope.
//
// Everything here is inert unless SMALL_MODEL_MODE is on; callers pass the
// relevant config values in explicitly so this package stays free of a
// dependency on pkg/config.
package smallmodel

import (
	"encoding/json"

	"github.com/vxcontrol/langchaingo/llms"

	"pentagi/pkg/cast"
)

// Budget decides when a chain must be compacted before the next model call. A
// small model degrades sharply past a fraction of its declared window, so the
// trigger is a token budget, not the raw byte thresholds the summarizer uses.
type Budget struct {
	// CtxWindow is the context window, in tokens, of the model the chain is sent to.
	CtxWindow int
	// Percent is the share of the window a chain may fill before compaction.
	Percent int
	// BytesPerToken is the divisor used to estimate tokens from bytes when no
	// tokenizer is available. English prose is ~4; JSON, IPs and hex are denser,
	// so the prudent default is 3.
	BytesPerToken int
	// CountTokens, when set, measures a chain exactly (e.g. via the local model
	// server's tokenizer) and is preferred over the byte estimate.
	CountTokens func([]llms.MessageContent) (int, error)
	// ReserveTokens is the part of the window the chain cannot use: the answer
	// the call asks for and the tool schemas it carries share the window with
	// the prompt. Zero reserves nothing.
	ReserveTokens int
}

// TokenLimit is the token count at which a chain must be compacted: the
// configured share of the window, lowered to what the reserve leaves free. A
// reserve that fills the whole window is ignored rather than forcing a limit of
// zero; the provider then lowers the answer to the room the prompt leaves.
func (b Budget) TokenLimit() int {
	limit := b.CtxWindow * b.Percent / 100
	if b.ReserveTokens <= 0 {
		return limit
	}
	if room := b.CtxWindow - b.ReserveTokens; room > 0 && room < limit {
		return room
	}
	return limit
}

// EstimateToolTokens estimates the tokens the tool schemas of a call take, using
// the same bytes-per-token divisor as the chain estimate.
func EstimateToolTokens(tools []llms.Tool, bytesPerToken int) int {
	if len(tools) == 0 {
		return 0
	}
	if bytesPerToken <= 0 {
		bytesPerToken = 3
	}
	schemas, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return len(schemas) / bytesPerToken
}

// Tokens returns the estimated or measured token count of a chain. It falls
// back to the byte estimate when CountTokens is unset or returns an error.
func (b Budget) Tokens(chain []llms.MessageContent) int {
	if b.CountTokens != nil {
		if n, err := b.CountTokens(chain); err == nil {
			return n
		}
	}
	return b.estimateTokens(chain)
}

func (b Budget) estimateTokens(chain []llms.MessageContent) int {
	perToken := b.BytesPerToken
	if perToken <= 0 {
		perToken = 3
	}
	bytes := 0
	for i := range chain {
		bytes += cast.CalculateMessageSize(&chain[i])
	}
	return bytes / perToken
}

// Exceeded reports whether a chain is over the token budget and should be
// compacted before the next model call. A non-positive window disables the
// check so the feature can be turned off without special-casing callers.
func (b Budget) Exceeded(chain []llms.MessageContent) bool {
	if b.CtxWindow <= 0 || b.Percent <= 0 {
		return false
	}
	return b.Tokens(chain) > b.TokenLimit()
}
