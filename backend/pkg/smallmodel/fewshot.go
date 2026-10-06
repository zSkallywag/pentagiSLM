package smallmodel

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Example is one input/output demonstration shown to the model. A small model
// learns a task shape from a few close examples better than from a long
// instruction.
type Example struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// ExampleSet is a pool of few-shot examples with their input token sets
// precomputed for selection.
type ExampleSet struct {
	examples []Example
	tokens   []map[string]struct{}
}

// LoadExamples reads a JSON array of {input, output} objects from path.
func LoadExamples(path string) (*ExampleSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var examples []Example
	if err := json.Unmarshal(data, &examples); err != nil {
		return nil, err
	}
	return NewExampleSet(examples), nil
}

// NewExampleSet precomputes the token set of each example's input.
func NewExampleSet(examples []Example) *ExampleSet {
	set := &ExampleSet{examples: examples, tokens: make([]map[string]struct{}, len(examples))}
	for i, e := range examples {
		set.tokens[i] = toSet(tokenize(e.Input))
	}
	return set
}

// Len reports how many examples the set holds; safe on a nil set.
func (s *ExampleSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.examples)
}

// Select returns up to k examples whose input is most similar to query, ranked
// by token-set overlap (Jaccard). Examples with no overlap are excluded, so an
// unrelated query yields nothing rather than noise. Ties break by original order
// for determinism.
func (s *ExampleSet) Select(query string, k int) []Example {
	if s == nil || len(s.examples) == 0 || k <= 0 {
		return nil
	}
	q := toSet(tokenize(query))
	if len(q) == 0 {
		return nil
	}

	type scored struct {
		idx   int
		score float64
	}
	ranked := make([]scored, 0, len(s.tokens))
	for i, toks := range s.tokens {
		if score := jaccard(q, toks); score > 0 {
			ranked = append(ranked, scored{i, score})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].idx < ranked[b].idx
	})

	if k > len(ranked) {
		k = len(ranked)
	}
	out := make([]Example, 0, k)
	for _, r := range ranked[:k] {
		out = append(out, s.examples[r.idx])
	}
	return out
}

// Block renders the selected examples into a prompt block, or "" when none match.
func (s *ExampleSet) Block(query string, k int) string {
	selected := s.Select(query, k)
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("RELEVANT EXAMPLES:\n")
	for _, e := range selected {
		b.WriteString("- input: ")
		b.WriteString(collapseSpaces(e.Input))
		b.WriteString("\n  output: ")
		b.WriteString(collapseSpaces(e.Output))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func toSet(tokens []string) map[string]struct{} {
	m := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		m[t] = struct{}{}
	}
	return m
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
