package smallmodel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exampleSet() *ExampleSet {
	return NewExampleSet([]Example{
		{Input: "scan the host for open ports with nmap", Output: "nmap -sV TARGET"},
		{Input: "brute force the ssh login", Output: "hydra -l user -P list ssh://TARGET"},
		{Input: "enumerate open ports and services on the target host", Output: "nmap -sV -p- TARGET"},
	})
}

func TestExampleSet_Select_RanksByTokenOverlap(t *testing.T) {
	got := exampleSet().Select("which open ports are on the host", 2)

	require.Len(t, got, 2)
	assert.Contains(t, got[0].Input, "ports")
	for _, e := range got {
		assert.NotContains(t, e.Input, "brute force")
	}
}

func TestExampleSet_Select_ExcludesZeroOverlap(t *testing.T) {
	got := exampleSet().Select("completely unrelated xyzzy query", 3)
	assert.Empty(t, got)
}

func TestExampleSet_Select_NilAndEmptyAreSafe(t *testing.T) {
	var nilSet *ExampleSet
	assert.Nil(t, nilSet.Select("ports", 3))
	assert.Equal(t, 0, nilSet.Len())
	assert.Empty(t, NewExampleSet(nil).Select("ports", 3))
}

func TestExampleSet_Block_RendersSelectedExamples(t *testing.T) {
	block := exampleSet().Block("open ports on the host", 1)
	assert.Contains(t, block, "RELEVANT EXAMPLES:")
	assert.Contains(t, block, "nmap")
}

func TestLoadExamples_ReadsJSONArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "examples.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"input":"scan ports","output":"nmap -sV TARGET"}]`), 0o600))

	set, err := LoadExamples(path)
	require.NoError(t, err)
	assert.Equal(t, 1, set.Len())
	assert.Contains(t, set.Block("scan ports", 1), "nmap -sV TARGET")
}

func TestLoadExamples_ErrorsOnMissingFile(t *testing.T) {
	_, err := LoadExamples(filepath.Join(t.TempDir(), "nope.json"))
	assert.Error(t, err)
}
