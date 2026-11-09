package smallmodel

import (
	"fmt"
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

func TestStore_AddFacts_DoesNotCountAnotherHostInAKnownNetAsProgress(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	st.RecordCommand(k, "curl -v https://www.example.com/")

	assert.Equal(t, 1, st.AddFacts(k, "host seen: 3.161.82.3"))
	_, since := st.RepetitionState(k)
	assert.Equal(t, 0, since, "the first host of a /24 is progress")

	st.RecordCommand(k, "curl -v https://www.example.com/")
	assert.Equal(t, 1, st.AddFacts(k, "host seen: 3.161.82.114"), "another host of the /24 is still recorded")
	_, since = st.RepetitionState(k)
	assert.Equal(t, 1, since, "another host of a known /24 is not progress")
	assert.Contains(t, st.Block(k), "host seen: 3.161.82.114")

	st.RecordCommand(k, "nmap -sn N.N.N.N/N")
	st.AddFacts(k, "host seen: 10.0.0.5")
	_, since = st.RepetitionState(k)
	assert.Equal(t, 0, since, "a host in a new /24 is progress")

	st.RecordCommand(k, "nmap -sV N.N.N.N")
	st.AddFacts(k, "host seen: 10.0.0.9", "443/tcp open")
	_, since = st.RepetitionState(k)
	assert.Equal(t, 0, since, "a new fact of another kind is progress beside a host that is not")
}

func TestStore_RepeatedFailure_AnswersOnlyACallThatFailedTheSameWayTwice(t *testing.T) {
	st := NewStore()
	const call = "terminal\ninput: curl -X POST /api/v3/transform\n"

	st.RecordCallResult(7, call, `{"code":400}`, true)
	_, ok := st.RepeatedFailure(7, call)
	assert.False(t, ok, "one failure is not a pattern")

	st.RecordCallResult(7, call, `{"code":400}`, true)
	answer, ok := st.RepeatedFailure(7, call)
	assert.True(t, ok)
	assert.Equal(t, `{"code":400}`, answer)
	_, ok = st.RepeatedFailure(8, call)
	assert.False(t, ok, "the record is per subtask")

	st.RecordCallResult(7, call, `{"code":500}`, true)
	_, ok = st.RepeatedFailure(7, call)
	assert.False(t, ok, "a different failure starts a new run")

	st.RecordCallResult(7, call, `{"code":500}`, true)
	answer, ok = st.RepeatedFailure(7, call)
	assert.True(t, ok)
	assert.Equal(t, `{"code":500}`, answer)

	st.RecordCallResult(7, call, `{"listingCards":[]}`, false)
	_, ok = st.RepeatedFailure(7, call)
	assert.False(t, ok, "a success clears the record")

	st.RecordCallResult(7, call, `{"listingCards":[]}`, false)
	_, ok = st.RepeatedFailure(7, call)
	assert.False(t, ok, "a call that keeps succeeding is never answered from the record")
}

func TestStore_RecordCallResult_StopsTakingNewCallsAtTheCap(t *testing.T) {
	st := NewStore()
	for i := 0; i < 256; i++ {
		st.RecordCallResult(7, fmt.Sprintf("call %d", i), "denied", true)
	}

	st.RecordCallResult(7, "one call too many", "denied", true)
	st.RecordCallResult(7, "one call too many", "denied", true)
	_, ok := st.RepeatedFailure(7, "one call too many")
	assert.False(t, ok, "a call past the cap runs as if unrecorded")

	st.RecordCallResult(7, "call 0", "denied", true)
	_, ok = st.RepeatedFailure(7, "call 0")
	assert.True(t, ok, "a call already recorded keeps its run")
}

func TestStore_TakeSnapshot_ReportsEachNewFactOnce(t *testing.T) {
	st := NewStore()
	k := Key{FlowID: 1, TaskID: 1}
	assert.Equal(t, Snapshot{}, st.TakeSnapshot(k), "an unknown task has nothing to report")

	st.AddFacts(k, "22/tcp open: ssh", "host seen: 10.0.0.5")
	st.RecordCommand(k, "ssh N.N.N.N")
	st.AddFailedTry(k, "ssh failed: Permission denied")
	st.BlockGesture(k, "hydra -l <str> N.N.N.N")

	first := st.TakeSnapshot(k)
	assert.Equal(t, Snapshot{
		Facts:             2,
		NewFacts:          []string{"22/tcp open: ssh", "host seen: 10.0.0.5"},
		FailedTries:       []string{"ssh failed: Permission denied"},
		RecentCommands:    []string{"ssh N.N.N.N"},
		CmdsSinceProgress: 1,
		BlockedGestures:   []string{"hydra -l <str> N.N.N.N"},
	}, first)

	st.AddFacts(k, "80/tcp open: http")
	second := st.TakeSnapshot(k)
	assert.Equal(t, 3, second.Facts)
	assert.Equal(t, []string{"80/tcp open: http"}, second.NewFacts, "only the facts since the last snapshot")

	assert.Empty(t, st.TakeSnapshot(k).NewFacts)
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
