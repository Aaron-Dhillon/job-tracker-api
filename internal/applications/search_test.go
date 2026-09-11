package applications_test

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/applications"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

func ownerID(t *testing.T) *uuid.UUID {
	t.Helper()
	id := uuid.New()
	return &id
}

func TestParseListParams_Defaults(t *testing.T) {
	p, err := applications.ParseListParams(url.Values{}, nil)
	require.NoError(t, err)

	assert.Equal(t, applications.DefaultLimit, p.Limit)
	assert.Zero(t, p.Offset)
	assert.Empty(t, p.Query)
	assert.Empty(t, string(p.Status))
	assert.Nil(t, p.OwnerID, "a nil owner means admin scope: every row")
}

// Over-max is capped rather than rejected. A client asking for 500 wants "as
// many as you'll give me"; a 400 there is pedantry. A negative limit is a
// different thing -- it is a bug in the caller, so it fails loudly.
func TestParseListParams_LimitAndOffset(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
		wantErr    string
	}{
		{name: "explicit values", query: "limit=5&offset=10", wantLimit: 5, wantOffset: 10},
		{name: "limit clamped to max", query: "limit=5000", wantLimit: applications.MaxLimit},
		{name: "limit exactly at max", query: "limit=100", wantLimit: 100},
		{name: "zero limit is honoured", query: "limit=0", wantLimit: 0},
		{name: "blank falls back to default", query: "limit=&offset=", wantLimit: applications.DefaultLimit},
		{name: "negative limit", query: "limit=-1", wantErr: "limit must not be negative"},
		{name: "negative offset", query: "offset=-1", wantErr: "offset must not be negative"},
		{name: "non-numeric limit", query: "limit=twenty", wantErr: "limit must be an integer"},
		{name: "non-numeric offset", query: "offset=x", wantErr: "offset must be an integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			require.NoError(t, err)

			p, err := applications.ParseListParams(values, nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantLimit, p.Limit)
			assert.Equal(t, tt.wantOffset, p.Offset)
		})
	}
}

func TestParseListParams_Status(t *testing.T) {
	for _, state := range workflow.AllStates() {
		t.Run(string(state), func(t *testing.T) {
			p, err := applications.ParseListParams(url.Values{"status": {string(state)}}, nil)
			require.NoError(t, err)
			assert.Equal(t, state, p.Status)
		})
	}

	_, err := applications.ParseListParams(url.Values{"status": {"bogus"}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown status")
}

func TestParseListParams_TrimsQuery(t *testing.T) {
	p, err := applications.ParseListParams(url.Values{"q": {"  staff engineer  "}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "staff engineer", p.Query)
}

// The plain path must not carry any of the search machinery: a union over two
// branches for a query nobody asked for is wasted work on every list request.
func TestBuildListQuery_NoSearch(t *testing.T) {
	sql, args := applications.BuildListQuery(applications.ListParams{Limit: 20, Offset: 0})

	assert.NotContains(t, sql, "union all")
	assert.NotContains(t, sql, "plainto_tsquery")
	assert.NotContains(t, sql, "ilike")
	assert.Contains(t, sql, "order by a.applied_on desc, a.id")
	assert.Contains(t, sql, "join companies c on c.id = a.company_id")
	assert.Equal(t, []any{20, 0}, args)
	assertPlaceholdersMatchArgs(t, sql, args)
}

// The union is the whole design. If it ever collapses back into one OR'd
// WHERE, the GIN index stops being usable and the DoD's EXPLAIN check fails.
func TestBuildListQuery_SearchUsesTwoRankedBranches(t *testing.T) {
	sql, args := applications.BuildListQuery(applications.ListParams{Query: "engineer", Limit: 20})

	assert.Contains(t, sql, "union all")
	assert.Contains(t, sql, "a.search_vec @@ plainto_tsquery('english', $1)",
		"the tsvector predicate must stand alone in its branch to stay index-eligible")
	assert.Contains(t, sql, "ts_rank(a.search_vec, plainto_tsquery('english', $1))")
	assert.Contains(t, sql, "c.name ilike $2")
	assert.Contains(t, sql, "1.0 as rank", "the company branch outranks incidental body-text mentions")
	assert.Contains(t, sql, "max(rank)", "a row matching both branches keeps the higher score")
	assert.Contains(t, sql, "order by r.rank desc, a.applied_on desc, a.id")

	// Nothing OR's the two predicates together in a single where.
	assert.NotRegexp(t, regexp.MustCompile(`(?i)@@[^)]*\)\s+or\s`), sql)

	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, "engineer", args[0], "the raw term goes to plainto_tsquery")
	assert.Equal(t, "%engineer%", args[1], "the escaped term goes to ilike")
	assertPlaceholdersMatchArgs(t, sql, args)
}

// Ownership belongs in the SQL, not in a post-filter in Go: a row the caller
// may not see should never be read, ranked, or counted.
func TestBuildListQuery_OwnershipScoping(t *testing.T) {
	owner := ownerID(t)

	for _, tt := range []struct {
		name  string
		query string
	}{
		{"plain list", ""},
		{"search", "engineer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scoped, scopedArgs := applications.BuildListQuery(applications.ListParams{
				Query: tt.query, Limit: 20, OwnerID: owner,
			})
			assert.Contains(t, scoped, "a.user_id = $")
			assert.Contains(t, scopedArgs, *owner)
			assertPlaceholdersMatchArgs(t, scoped, scopedArgs)

			admin, adminArgs := applications.BuildListQuery(applications.ListParams{
				Query: tt.query, Limit: 20,
			})
			assert.NotContains(t, admin, "a.user_id =", "a nil owner must not scope the query")
			assertPlaceholdersMatchArgs(t, admin, adminArgs)
		})
	}
}

// Both branches of the union have to carry the filters. Applying them outside
// the union would rank rows the caller cannot see and then throw them away,
// and would put an access-control rule one level away from the rows it guards.
func TestBuildListQuery_SearchFiltersBothBranches(t *testing.T) {
	owner := ownerID(t)
	sql, args := applications.BuildListQuery(applications.ListParams{
		Query: "engineer", Status: workflow.StateOffer, Limit: 20, OwnerID: owner,
	})

	branches := strings.SplitN(sql, "union all", 2)
	require.Len(t, branches, 2, "expected exactly one union all")

	for i, branch := range branches {
		assert.Contains(t, branch, "a.user_id = $", "branch %d must be ownership-scoped", i+1)
		assert.Contains(t, branch, "a.status = $", "branch %d must carry the status filter", i+1)
	}
	assert.Contains(t, args, string(workflow.StateOffer))
	assertPlaceholdersMatchArgs(t, sql, args)
}

// Wildcards are escaped because of what they mean, not because of injection --
// the value is already a bind parameter. Without escaping, searching "100%"
// matches every company and the feature looks broken.
func TestBuildListQuery_EscapesLikeWildcards(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"100%", `%100\%%`},
		{"a_b", `%a\_b%`},
		{`back\slash`, `%back\\slash%`},
		{"plain", "%plain%"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			_, args := applications.BuildListQuery(applications.ListParams{Query: tt.query, Limit: 20})
			require.GreaterOrEqual(t, len(args), 2)
			assert.Equal(t, tt.want, args[1])
			assert.Equal(t, tt.query, args[0], "the tsquery argument stays unescaped")
		})
	}
}

// The builder assembles placeholders as it appends arguments, so a condition
// added or skipped in the wrong order silently shifts every later $n. This
// walks the whole parameter space and checks the two can never disagree.
func TestBuildListQuery_PlaceholderNumberingHolds(t *testing.T) {
	owner := ownerID(t)

	for _, query := range []string{"", "engineer"} {
		for _, status := range []workflow.State{"", workflow.StateApplied} {
			for _, scope := range []*uuid.UUID{nil, owner} {
				name := fmt.Sprintf("q=%q status=%q scoped=%t", query, status, scope != nil)
				t.Run(name, func(t *testing.T) {
					sql, args := applications.BuildListQuery(applications.ListParams{
						Query: query, Status: status, Limit: 20, Offset: 5, OwnerID: scope,
					})
					assertPlaceholdersMatchArgs(t, sql, args)
					// limit and offset are always the last two bound.
					require.GreaterOrEqual(t, len(args), 2)
					assert.Equal(t, 20, args[len(args)-2])
					assert.Equal(t, 5, args[len(args)-1])
				})
			}
		}
	}
}

// assertPlaceholdersMatchArgs checks that the set of $n referenced by the SQL
// is exactly 1..len(args): no placeholder points past the arguments, and no
// argument is bound but never used.
func assertPlaceholdersMatchArgs(t *testing.T, sql string, args []any) {
	t.Helper()

	seen := map[int]bool{}
	for _, match := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(sql, -1) {
		n, err := strconv.Atoi(match[1])
		require.NoError(t, err)
		seen[n] = true
	}

	used := make([]int, 0, len(seen))
	for n := range seen {
		used = append(used, n)
	}
	sort.Ints(used)

	want := make([]int, 0, len(args))
	for i := range args {
		want = append(want, i+1)
	}
	assert.Equal(t, want, used, "placeholders in the SQL must line up exactly with the bound arguments\nSQL:\n%s", sql)
}
