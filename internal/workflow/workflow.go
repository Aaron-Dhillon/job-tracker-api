// Package workflow implements the job application status state machine.
//
// It deliberately depends on nothing but errors: no database, no HTTP, no
// third-party packages. The transition table below is the entire business
// rule, and handlers consult it before writing anything, so an illegal move is
// rejected without ever touching a row.
package workflow

import "errors"

// State is an application status. The values match the check constraint on
// applications.status.
type State string

const (
	StateApplied      State = "applied"
	StateScreening    State = "screening"
	StateInterviewing State = "interviewing"
	StateOffer        State = "offer"
	StateRejected     State = "rejected"
	StateWithdrawn    State = "withdrawn"
)

var (
	// ErrInvalidTransition means both states are real but the move between
	// them is not permitted. Handlers map this to HTTP 409.
	ErrInvalidTransition = errors.New("invalid transition")

	// ErrUnknownState means one of the states is not part of the machine.
	// Handlers map this to HTTP 400.
	ErrUnknownState = errors.New("unknown state")
)

// allStates enumerates every state independently of the transition table, so
// the table-integrity test is not checking the table against itself.
var allStates = []State{
	StateApplied,
	StateScreening,
	StateInterviewing,
	StateOffer,
	StateRejected,
	StateWithdrawn,
}

// allowed maps each state to the states reachable from it in one step.
//
// rejected and withdrawn are terminal, so they have no outgoing edges. Every
// non-terminal state can reach both of them: an application can be turned down
// or given up at any point before it ends.
var allowed = map[State][]State{
	StateApplied:      {StateScreening, StateRejected, StateWithdrawn},
	StateScreening:    {StateInterviewing, StateRejected, StateWithdrawn},
	StateInterviewing: {StateOffer, StateRejected, StateWithdrawn},
	StateOffer:        {StateRejected, StateWithdrawn}, // accepting an offer is out of scope
	StateRejected:     {},
	StateWithdrawn:    {},
}

// Transition reports whether an application may move from one state to
// another. It returns nil when the move is permitted, ErrUnknownState when
// either state is not part of the machine, and ErrInvalidTransition when both
// are real but the edge does not exist.
//
// A state never transitions to itself: no state lists itself as reachable.
func Transition(from, to State) error {
	if !Valid(from) || !Valid(to) {
		return ErrUnknownState
	}
	for _, next := range allowed[from] {
		if next == to {
			return nil
		}
	}
	return ErrInvalidTransition
}

// Valid reports whether s is a state of this machine.
func Valid(s State) bool {
	_, ok := allowed[s]
	return ok
}

// IsTerminal reports whether s is a real state with no outgoing transitions.
// An unknown state is not terminal, it is simply not a state.
func IsTerminal(s State) bool {
	next, ok := allowed[s]
	return ok && len(next) == 0
}

// Next returns the states reachable from s in one step, for building 409
// bodies and the README diagram. The result is a fresh slice, so callers
// cannot mutate the transition table through it.
//
// A known state always returns a non-nil slice, empty for a terminal state, so
// the "allowed" field of a 409 body marshals as [] rather than null. nil is
// reserved for a state that does not exist.
func Next(s State) []State {
	next, ok := allowed[s]
	if !ok {
		return nil
	}
	return append(make([]State, 0, len(next)), next...)
}

// AllStates returns every state, in workflow order. The result is a copy.
func AllStates() []State {
	return append([]State(nil), allStates...)
}
