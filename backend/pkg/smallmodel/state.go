package smallmodel

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// State is the structured working memory of a pentest task. It is built by
// deterministic code from tool output, never by the small model, and is placed
// at the top of the prompt on every turn. Anything that must survive chain
// compaction lives here, not in the conversation history.
type State struct {
	Objective   string
	Facts       []string
	FailedTries []string
	NextStep    string

	// Loop-detection bookkeeping, never serialized into the prompt block:
	// recentCanon is a ring of the last canonical commands, and
	// cmdsSinceProgress counts commands run since the last new fact was learned.
	recentCanon       []string
	cmdsSinceProgress int

	// Recovery escalation: armedGesture/armedCount track how many turns in a row
	// the recovery directive has fired on the same gesture (the model ignoring
	// it), and blockedGestures are canonical gestures disabled after that count
	// crossed the escalation threshold.
	armedGesture    string
	armedCount      int
	blockedGestures map[string]struct{}
}

// recentCanonCap bounds the ring of canonical commands kept for loop detection.
const recentCanonCap = 16

// ToBlock serializes the state into the fixed block that heads the prompt. The
// labels are stable so a small model learns the shape; an empty state yields an
// empty string so nothing is injected before there is anything to say.
func (s State) ToBlock() string {
	if s.Objective == "" && len(s.Facts) == 0 && len(s.FailedTries) == 0 && s.NextStep == "" {
		return ""
	}

	var b strings.Builder
	if s.Objective != "" {
		fmt.Fprintf(&b, "OBJECTIVE: %s\n", s.Objective)
	}
	b.WriteString("ESTABLISHED FACTS:\n")
	for _, f := range s.Facts {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("FAILED ATTEMPTS:\n")
	for _, f := range s.FailedTries {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	if s.NextStep != "" {
		fmt.Fprintf(&b, "NEXT STEP: %s\n", s.NextStep)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Key identifies the task a State belongs to. Subtasks of the same task share
// the task-level state so facts found in one carry into the next.
type Key struct {
	FlowID int64
	TaskID int64
}

// Store holds per-task state and is safe for concurrent use: tool handlers on
// different workers update it while the agent loop reads it.
type Store struct {
	mu        sync.RWMutex
	states    map[Key]*State
	gateFired map[Key]struct{}
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		states:    make(map[Key]*State),
		gateFired: make(map[Key]struct{}),
	}
}

func (st *Store) stateLocked(k Key) *State {
	s, ok := st.states[k]
	if !ok {
		s = &State{}
		st.states[k] = s
	}
	return s
}

// Block returns the serialized state for a key, or an empty string if none.
func (st *Store) Block(k Key) string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if s, ok := st.states[k]; ok {
		return s.ToBlock()
	}
	return ""
}

// SetObjective records the task objective.
func (st *Store) SetObjective(k Key, objective string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stateLocked(k).Objective = strings.TrimSpace(objective)
}

// SetNextStep records the next planned step.
func (st *Store) SetNextStep(k Key, next string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stateLocked(k).NextStep = strings.TrimSpace(next)
}

// AddFacts appends facts, de-duplicating against what is already recorded, and
// returns how many were genuinely new. A new fact is progress, so it resets the
// no-progress counter the loop detector reads.
func (st *Store) AddFacts(k Key, facts ...string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	before := len(s.Facts)
	s.Facts = appendUnique(s.Facts, facts)
	added := len(s.Facts) - before
	if added > 0 {
		s.cmdsSinceProgress = 0
	}
	return added
}

// RecordCommand appends a canonical command to the per-task ring and counts it as
// one more command since the last progress. Called before each terminal command
// that passed the pre-execution guards.
func (st *Store) RecordCommand(k Key, canonical string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	s.recentCanon = append(s.recentCanon, canonical)
	if len(s.recentCanon) > recentCanonCap {
		s.recentCanon = s.recentCanon[len(s.recentCanon)-recentCanonCap:]
	}
	s.cmdsSinceProgress++
}

// NoteRecoveryArmed records that the recovery directive fired on a gesture this
// turn and returns how many turns in a row it has fired on that same gesture —
// a rising count means the model keeps ignoring it. A different gesture resets
// the count to 1.
func (st *Store) NoteRecoveryArmed(k Key, gesture string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	if s.armedGesture == gesture {
		s.armedCount++
	} else {
		s.armedGesture = gesture
		s.armedCount = 1
	}
	return s.armedCount
}

// BlockGesture disables a canonical gesture for a task, so the pre-execution
// guard rejects it instead of running it.
func (st *Store) BlockGesture(k Key, gesture string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	if s.blockedGestures == nil {
		s.blockedGestures = make(map[string]struct{})
	}
	s.blockedGestures[gesture] = struct{}{}
}

// IsGestureBlocked reports whether a canonical gesture is disabled for a task.
func (st *Store) IsGestureBlocked(k Key, gesture string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.states[k]
	if !ok {
		return false
	}
	_, blocked := s.blockedGestures[gesture]
	return blocked
}

// RepetitionState returns a copy of the recent canonical commands and the number
// of commands run since the last new fact, for DetectStuckByRepetition.
func (st *Store) RepetitionState(k Key) (recent []string, cmdsSinceProgress int) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.states[k]
	if !ok {
		return nil, 0
	}
	recent = make([]string, len(s.recentCanon))
	copy(recent, s.recentCanon)
	return recent, s.cmdsSinceProgress
}

// AddFailedTry records a failed attempt, de-duplicating.
func (st *Store) AddFailedTry(k Key, try string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	s.FailedTries = appendUnique(s.FailedTries, []string{try})
}

// FailedTries returns a copy of the recorded failed attempts for a key, for
// loop detection. The copy keeps callers from racing the store's mutations.
func (st *Store) FailedTries(k Key) []string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.states[k]
	if !ok {
		return nil
	}
	out := make([]string, len(s.FailedTries))
	copy(out, s.FailedTries)
	return out
}

// Facts returns a copy of the established facts for a key, for the strategy gate.
func (st *Store) Facts(k Key) []string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.states[k]
	if !ok {
		return nil
	}
	out := make([]string, len(s.Facts))
	copy(out, s.Facts)
	return out
}

// MarkStrategyGateFired records that the strategy gate has already prompted this
// task once, so the agent is asked to reconsider a wordlist brute-force a single
// time and then trusted — the gate never blocks the same task twice.
func (st *Store) MarkStrategyGateFired(k Key) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.gateFired[k] = struct{}{}
}

// StrategyGateFired reports whether the gate has already fired for this task.
func (st *Store) StrategyGateFired(k Key) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	_, ok := st.gateFired[k]
	return ok
}

// Reset drops the state for a key, used when a task is torn down.
func (st *Store) Reset(k Key) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.states, k)
	delete(st.gateFired, k)
}

func appendUnique(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		seen[e] = struct{}{}
	}
	for _, in := range incoming {
		in = strings.TrimSpace(in)
		if in == "" {
			continue
		}
		if _, dup := seen[in]; dup {
			continue
		}
		seen[in] = struct{}{}
		existing = append(existing, in)
	}
	return existing
}

// MergeFacts is a convenience for feeding deterministic extraction output into
// the store in a stable order.
func (st *Store) MergeFacts(k Key, facts []string) {
	sorted := make([]string, len(facts))
	copy(sorted, facts)
	sort.Strings(sorted)
	st.AddFacts(k, sorted...)
}
