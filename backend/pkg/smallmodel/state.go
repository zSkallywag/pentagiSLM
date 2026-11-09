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

	// seenNets holds the /24 of every host fact recorded, so a further address in
	// a known /24 is recorded without counting as progress.
	seenNets map[string]struct{}
	// factsSnapshotted is how many Facts the last Snapshot already reported.
	factsSnapshotted int
}

// recentCanonCap bounds the ring of canonical commands kept for loop detection.
const recentCanonCap = 16

// callRecordCap bounds the failed calls remembered per subtask; calls beyond it
// simply run, as they would without the record.
const callRecordCap = 256

// repeatedFailureThreshold is how many identical failures a call may get before
// it is answered from the record instead of being run again.
const repeatedFailureThreshold = 2

// callRecord is the last failed answer a call got in a subtask and how many
// times in a row it got exactly that answer.
type callRecord struct {
	answer string
	count  int
}

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
	// subtaskCmds counts terminal commands per subtask id, for the budget cap.
	subtaskCmds map[int64]int
	// failedCalls holds, per subtask id, the failed answers of calls keyed by the
	// caller's canonical form of the call.
	failedCalls map[int64]map[string]*callRecord
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		states:      make(map[Key]*State),
		gateFired:   make(map[Key]struct{}),
		subtaskCmds: make(map[int64]int),
		failedCalls: make(map[int64]map[string]*callRecord),
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
// no-progress counter the loop detector reads — except a host in a /24 already
// seen: every verbose HTTP client prints the address it connected to, and a CDN
// rotates that address on each request, so counting each one would reset the
// counter on every repeat of the same request.
func (st *Store) AddFacts(k Key, facts ...string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.stateLocked(k)
	before := len(s.Facts)
	s.Facts = appendUnique(s.Facts, facts)
	for _, fact := range s.Facts[before:] {
		if s.isProgressLocked(fact) {
			s.cmdsSinceProgress = 0
		}
	}
	return len(s.Facts) - before
}

// isProgressLocked reports whether a newly recorded fact is progress, recording
// the /24 of a host fact so the next address in it is not.
func (s *State) isProgressLocked(fact string) bool {
	addr, ok := strings.CutPrefix(fact, HostFactPrefix)
	if !ok {
		return true
	}
	network := addr
	if i := strings.LastIndexByte(addr, '.'); i >= 0 {
		network = addr[:i]
	}
	if _, seen := s.seenNets[network]; seen {
		return false
	}
	if s.seenNets == nil {
		s.seenNets = make(map[string]struct{})
	}
	s.seenNets[network] = struct{}{}
	return true
}

// RecordCommand appends a canonical command to the per-task ring and counts it as
// one more command since the last progress. Called before each terminal command
// or file call that passed the pre-execution guards.
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

// IncrSubtaskCommands increments and returns the terminal-command count for a
// subtask, for the per-subtask budget cap.
func (st *Store) IncrSubtaskCommands(subtaskID int64) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.subtaskCmds[subtaskID]++
	return st.subtaskCmds[subtaskID]
}

// SubtaskCommands returns the terminal-command count recorded for a subtask.
func (st *Store) SubtaskCommands(subtaskID int64) int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.subtaskCmds[subtaskID]
}

// RecordCallResult files the answer a call got in a subtask. call is the caller's
// canonical form of the call. A failed answer identical to the last one recorded
// for the call extends its run; any other failed answer starts a new run, and a
// successful answer clears the record, since the call no longer fails the same way.
func (st *Store) RecordCallResult(subtaskID int64, call, answer string, failed bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	calls := st.failedCalls[subtaskID]
	if !failed {
		delete(calls, call)
		return
	}
	if calls == nil {
		calls = make(map[string]*callRecord)
		st.failedCalls[subtaskID] = calls
	}
	rec, ok := calls[call]
	if !ok {
		if len(calls) >= callRecordCap {
			return
		}
		calls[call] = &callRecord{answer: answer, count: 1}
		return
	}
	if rec.answer == answer {
		rec.count++
		return
	}
	rec.answer, rec.count = answer, 1
}

// RepeatedFailure returns the answer a call keeps failing with when it has failed
// with that same answer at least twice in the subtask, so the caller can answer
// from the record instead of running the call a third time.
func (st *Store) RepeatedFailure(subtaskID int64, call string) (string, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	rec, ok := st.failedCalls[subtaskID][call]
	if !ok || rec.count < repeatedFailureThreshold {
		return "", false
	}
	return rec.answer, true
}

// Snapshot is the loop-detection view of a task's state, for logging: the
// counts and lists that decide whether a recovery directive fires.
type Snapshot struct {
	Facts             int
	NewFacts          []string
	FailedTries       []string
	RecentCommands    []string
	CmdsSinceProgress int
	BlockedGestures   []string
}

// TakeSnapshot returns the state of a task for logging. NewFacts holds only the
// facts recorded since the previous snapshot of the task, so a log written on
// every turn shows each fact once, at the turn it appeared.
func (st *Store) TakeSnapshot(k Key) Snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.states[k]
	if !ok {
		return Snapshot{}
	}
	snap := Snapshot{
		Facts:             len(s.Facts),
		NewFacts:          append([]string(nil), s.Facts[min(s.factsSnapshotted, len(s.Facts)):]...),
		FailedTries:       append([]string(nil), s.FailedTries...),
		RecentCommands:    append([]string(nil), s.recentCanon...),
		CmdsSinceProgress: s.cmdsSinceProgress,
	}
	for gesture := range s.blockedGestures {
		snap.BlockedGestures = append(snap.BlockedGestures, gesture)
	}
	sort.Strings(snap.BlockedGestures)
	s.factsSnapshotted = len(s.Facts)
	return snap
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
