package workflow_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// allowedPairs is the PRD's transition table, retyped by hand rather than
// derived from the package. A test that reads the same map it is checking
// proves nothing; this one fails if the shipped table drifts from the spec.
//
//	applied      -> screening, rejected, withdrawn
//	screening    -> interviewing, rejected, withdrawn
//	interviewing -> offer, rejected, withdrawn
//	offer        -> rejected, withdrawn
//	rejected     -> (terminal)
//	withdrawn    -> (terminal)
var allowedPairs = map[[2]workflow.State]bool{
	{workflow.StateApplied, workflow.StateScreening}:      true,
	{workflow.StateApplied, workflow.StateRejected}:       true,
	{workflow.StateApplied, workflow.StateWithdrawn}:      true,
	{workflow.StateScreening, workflow.StateInterviewing}: true,
	{workflow.StateScreening, workflow.StateRejected}:     true,
	{workflow.StateScreening, workflow.StateWithdrawn}:    true,
	{workflow.StateInterviewing, workflow.StateOffer}:     true,
	{workflow.StateInterviewing, workflow.StateRejected}:  true,
	{workflow.StateInterviewing, workflow.StateWithdrawn}: true,
	{workflow.StateOffer, workflow.StateRejected}:         true,
	{workflow.StateOffer, workflow.StateWithdrawn}:        true,
}

// TestTransition_FullMatrix walks all 6x6 ordered pairs and asserts each one
// against the table above: 11 permitted moves and 25 rejections, which covers
// every terminal origin and every self-transition without singling them out.
func TestTransition_FullMatrix(t *testing.T) {
	states := workflow.AllStates()
	require.Len(t, states, 6, "the machine should have exactly six states")
	require.Len(t, allowedPairs, 11, "the PRD table has eleven edges")

	var permitted int
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				err := workflow.Transition(from, to)
				if allowedPairs[[2]workflow.State{from, to}] {
					assert.NoError(t, err)
					return
				}
				assert.ErrorIs(t, err, workflow.ErrInvalidTransition)
			})
			if allowedPairs[[2]workflow.State{from, to}] {
				permitted++
			}
		}
	}
	assert.Equal(t, 11, permitted, "every listed edge should be reachable in the matrix")
}

func TestTransition_TerminalStatesGoNowhere(t *testing.T) {
	for _, from := range []workflow.State{workflow.StateRejected, workflow.StateWithdrawn} {
		for _, to := range workflow.AllStates() {
			err := workflow.Transition(from, to)
			assert.ErrorIs(t, err, workflow.ErrInvalidTransition,
				"%s is terminal and must not reach %s", from, to)
		}
	}
}

func TestTransition_SelfTransitionRejected(t *testing.T) {
	for _, s := range workflow.AllStates() {
		assert.ErrorIs(t, workflow.Transition(s, s), workflow.ErrInvalidTransition,
			"%s should not transition to itself", s)
	}
}

func TestTransition_UnknownState(t *testing.T) {
	tests := []struct {
		name     string
		from, to workflow.State
	}{
		{"unknown from", "bogus", workflow.StateScreening},
		{"unknown to", workflow.StateApplied, "bogus"},
		{"both unknown", "bogus", "nonsense"},
		{"empty from", "", workflow.StateApplied},
		{"empty to", workflow.StateApplied, ""},
		{"wrong case", "Applied", workflow.StateScreening},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := workflow.Transition(tt.from, tt.to)
			assert.ErrorIs(t, err, workflow.ErrUnknownState)
			assert.NotErrorIs(t, err, workflow.ErrInvalidTransition,
				"an unknown state is a bad request, not a rejected move")
		})
	}
}

func TestValid(t *testing.T) {
	for _, s := range workflow.AllStates() {
		assert.True(t, workflow.Valid(s), "%s should be valid", s)
	}
	for _, s := range []workflow.State{"", "bogus", "Applied", "applied "} {
		assert.False(t, workflow.Valid(s), "%q should not be valid", s)
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := map[workflow.State]bool{
		workflow.StateRejected:  true,
		workflow.StateWithdrawn: true,
	}
	for _, s := range workflow.AllStates() {
		assert.Equal(t, terminal[s], workflow.IsTerminal(s), "IsTerminal(%s)", s)
	}

	// An unknown state has no outgoing edges either, but it is not terminal --
	// it is not a state at all. Guards against a bare len(allowed[s]) == 0.
	assert.False(t, workflow.IsTerminal("bogus"))
	assert.False(t, workflow.IsTerminal(""))
}

func TestNext(t *testing.T) {
	assert.Equal(t,
		[]workflow.State{workflow.StateScreening, workflow.StateRejected, workflow.StateWithdrawn},
		workflow.Next(workflow.StateApplied))

	// A terminal state has no moves but is still a state: empty, not nil.
	// An unknown state is nil. The distinction is what keeps "allowed" from
	// marshalling as null in a 409 body.
	assert.Empty(t, workflow.Next(workflow.StateRejected))
	assert.NotNil(t, workflow.Next(workflow.StateRejected))
	assert.Nil(t, workflow.Next("bogus"))
}

// TestNext_MarshalsForErrorBody locks the shape the transition handler returns
// on 409:
//
//	{"error": "invalid transition", "from": "offer", "allowed": ["rejected", "withdrawn"]}
func TestNext_MarshalsForErrorBody(t *testing.T) {
	body := func(from workflow.State) string {
		b, err := json.Marshal(map[string]any{
			"error":   workflow.ErrInvalidTransition.Error(),
			"from":    from,
			"allowed": workflow.Next(from),
		})
		require.NoError(t, err)
		return string(b)
	}

	assert.JSONEq(t,
		`{"error":"invalid transition","from":"offer","allowed":["rejected","withdrawn"]}`,
		body(workflow.StateOffer))

	// The commonest 409 is a move out of a terminal state. "allowed" must be
	// an empty array there, never null.
	assert.JSONEq(t,
		`{"error":"invalid transition","from":"rejected","allowed":[]}`,
		body(workflow.StateRejected))
}

func TestNext_ReturnsCopy(t *testing.T) {
	got := workflow.Next(workflow.StateApplied)
	require.NotEmpty(t, got)
	got[0] = "tampered"

	assert.Equal(t, workflow.StateScreening, workflow.Next(workflow.StateApplied)[0],
		"Next must not hand out the transition table itself")
	assert.NoError(t, workflow.Transition(workflow.StateApplied, workflow.StateScreening))
}

func TestAllStates_ReturnsCopy(t *testing.T) {
	got := workflow.AllStates()
	require.NotEmpty(t, got)
	got[0] = "tampered"

	assert.Equal(t, workflow.StateApplied, workflow.AllStates()[0])
}

// TestTable_Integrity catches a typo'd or malformed transition table: an
// unreachable state, an edge pointing at something that is not a state, a
// duplicate, or an accidental self-loop.
func TestTable_Integrity(t *testing.T) {
	states := workflow.AllStates()

	seen := make(map[workflow.State]bool, len(states))
	for _, s := range states {
		assert.False(t, seen[s], "%s is listed twice in AllStates", s)
		seen[s] = true
		assert.True(t, workflow.Valid(s), "%s is enumerated but missing from the table", s)
	}

	for _, from := range states {
		next := workflow.Next(from)

		edges := make(map[workflow.State]bool, len(next))
		for _, to := range next {
			assert.True(t, workflow.Valid(to), "%s -> %s targets an unknown state", from, to)
			assert.NotEqual(t, from, to, "%s has a self-loop", from)
			assert.False(t, edges[to], "%s -> %s is duplicated", from, to)
			edges[to] = true
		}

		if workflow.IsTerminal(from) {
			assert.Empty(t, next, "%s is terminal and must have no edges", from)
		} else {
			assert.NotEmpty(t, next, "%s is not terminal so it needs at least one edge", from)
		}
	}
}
