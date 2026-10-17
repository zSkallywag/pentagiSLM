package smallmodel

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestState_ToBlock_EmptyStateYieldsEmptyString(t *testing.T) {
	assert.Equal(t, "", State{}.ToBlock())
}

func TestState_ToBlock_RendersAllSections(t *testing.T) {
	s := State{
		Objective:   "authorized test on 10.0.0.5",
		Facts:       []string{"port 8443 open, Tomcat 9.0.31", "/manager reachable, needs auth"},
		FailedTries: []string{"default creds on /manager -> 401"},
		NextStep:    "check known CVEs for Tomcat 9.0.31",
	}
	block := s.ToBlock()

	assert.Contains(t, block, "OBJECTIVE: authorized test on 10.0.0.5")
	assert.Contains(t, block, "  - port 8443 open, Tomcat 9.0.31")
	assert.Contains(t, block, "FAILED ATTEMPTS:")
	assert.Contains(t, block, "  - default creds on /manager -> 401")
	assert.Contains(t, block, "NEXT STEP: check known CVEs for Tomcat 9.0.31")
	assert.False(t, strings.HasSuffix(block, "\n"))
}

func TestStore_AddFacts_DeduplicatesAndSkipsBlanks(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 2}

	st.AddFacts(k, "port 22 open", "port 22 open", "  ", "port 80 open")
	block := st.Block(k)

	assert.Equal(t, 1, strings.Count(block, "port 22 open"))
	assert.Contains(t, block, "port 80 open")
}

func TestStore_Block_IsEmptyForUnknownKey(t *testing.T) {
	st := NewStore()
	assert.Equal(t, "", st.Block(Key{FlowID: 9, TaskID: 9}))
}

func TestStore_Reset_DropsState(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	st.SetObjective(k, "x")
	st.Reset(k)
	assert.Equal(t, "", st.Block(k))
}

func TestStore_RecordCommand_CountsCommandsAndKeepsRing(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	for i := 0; i < recentCanonCap+5; i++ {
		st.RecordCommand(k, "python3 /work/*.py")
	}
	recent, since := st.RepetitionState(k)
	assert.Len(t, recent, recentCanonCap, "ring is capped")
	assert.Equal(t, recentCanonCap+5, since, "every command counts since no fact was added")
}

func TestStore_AddFacts_ResetsProgressOnlyForNewFacts(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	st.RecordCommand(k, "a")
	st.RecordCommand(k, "a")

	added := st.AddFacts(k, "22/tcp open: ssh")
	assert.Equal(t, 1, added)
	_, since := st.RepetitionState(k)
	assert.Equal(t, 0, since, "a new fact is progress and resets the counter")

	st.RecordCommand(k, "a")
	again := st.AddFacts(k, "22/tcp open: ssh") // duplicate, not new
	assert.Equal(t, 0, again)
	_, since = st.RepetitionState(k)
	assert.Equal(t, 1, since, "a duplicate fact is not progress and does not reset")
}

func TestStore_NoteRecoveryArmed_CountsConsecutiveSameGesture(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	assert.Equal(t, 1, st.NoteRecoveryArmed(k, "grep /work/*.log"))
	assert.Equal(t, 2, st.NoteRecoveryArmed(k, "grep /work/*.log"))
	assert.Equal(t, 3, st.NoteRecoveryArmed(k, "grep /work/*.log"))
	assert.Equal(t, 1, st.NoteRecoveryArmed(k, "ls /work/*"), "a different gesture resets the count")
}

func TestStore_BlockGesture_RejectsOnlyTheBlockedGesture(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	st.BlockGesture(k, "python3 /work/*.py")
	assert.True(t, st.IsGestureBlocked(k, "python3 /work/*.py"))
	assert.False(t, st.IsGestureBlocked(k, "nmap -sV N.N.N.N"))
	assert.False(t, st.IsGestureBlocked(Key{FlowID: 1, TaskID: 2}, "python3 /work/*.py"), "block is per-task")
}

func TestStore_SubtaskCommands_CountsPerSubtask(t *testing.T) {
	st := NewStore()
	assert.Equal(t, 0, st.SubtaskCommands(7))
	assert.Equal(t, 1, st.IncrSubtaskCommands(7))
	assert.Equal(t, 2, st.IncrSubtaskCommands(7))
	assert.Equal(t, 1, st.IncrSubtaskCommands(8), "a different subtask counts separately")
	assert.Equal(t, 2, st.SubtaskCommands(7))
}

func TestStore_ConcurrentWritesAreSafe(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st.AddFacts(k, string(rune('a'+i%26))+"-fact")
		}(i)
	}
	wg.Wait()

	assert.NotEmpty(t, st.Block(k))
}
