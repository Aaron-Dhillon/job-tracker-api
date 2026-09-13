package applications

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// Pagination bounds. Over-max is clamped rather than rejected: a client asking
// for 500 wants "as many as you'll give me", and a 400 there is pedantry.
// Negatives are a different thing -- they are a bug in the caller, so they 400.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// selectColumns is the projection every read shares. The indentation matches
// the query bodies below, so a statement logged by Postgres is readable.
const selectColumns = `
			a.id, a.user_id, a.company_id, c.name,
			a.role_title, a.location, a.notes, a.status,
			a.applied_on, a.created_at, a.updated_at`

// ListParams is a parsed, validated GET /applications query string.
type ListParams struct {
	Query  string         // ?q=, already trimmed
	Status workflow.State // ?status=, empty when absent
	Limit  int
	Offset int

	// OwnerID scopes the query to one user. Nil means no scoping, which is how
	// an admin sees every row -- and is why it is a pointer rather than a
	// uuid.UUID: the zero UUID is a value, not an absence, and would silently
	// match nothing instead of matching everything.
	OwnerID *uuid.UUID
}

// ParseListParams reads the query string, applying the documented defaults.
func ParseListParams(v url.Values, ownerID *uuid.UUID) (ListParams, error) {
	p := ListParams{Limit: DefaultLimit, OwnerID: ownerID}

	p.Query = strings.TrimSpace(v.Get("q"))

	if raw := strings.TrimSpace(v.Get("status")); raw != "" {
		state := workflow.State(raw)
		if !workflow.Valid(state) {
			return ListParams{}, fmt.Errorf("unknown status %q", raw)
		}
		p.Status = state
	}

	if raw := strings.TrimSpace(v.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return ListParams{}, fmt.Errorf("limit must be an integer")
		}
		if limit < 0 {
			return ListParams{}, fmt.Errorf("limit must not be negative")
		}
		if limit > MaxLimit {
			limit = MaxLimit
		}
		p.Limit = limit
	}

	if raw := strings.TrimSpace(v.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			return ListParams{}, fmt.Errorf("offset must be an integer")
		}
		if offset < 0 {
			return ListParams{}, fmt.Errorf("offset must not be negative")
		}
		p.Offset = offset
	}

	return p, nil
}

// args accumulates bind parameters and hands back their placeholders, so the
// numbering cannot drift out of step with the slice as conditions are added
// or skipped.
type args struct {
	values []any
}

func (a *args) add(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// BuildListQuery returns the SQL and bind arguments for a list request.
//
// It is exported so the integration suite can EXPLAIN the exact query the API
// runs. An index proof against a hand-copied approximation of the query proves
// nothing about the query that actually ships.
//
// The conditions are assembled as strings rather than written once with
// ($n::text is null or ...) guards. That is not a style preference: Postgres
// cannot use the GIN index through a parameterised null guard, because the
// planner has to produce one plan that works whether or not the parameter is
// null. A guarded query silently becomes a sequential scan, and the DoD
// requires EXPLAIN to show a Bitmap Index Scan.
func BuildListQuery(p ListParams) (string, []any) {
	var a args

	if p.Query == "" {
		return buildPlainList(p, &a), a.values
	}
	return buildSearchList(p, &a), a.values
}

// buildPlainList is the no-search path: filter, order, paginate.
func buildPlainList(p ListParams, a *args) string {
	var sb strings.Builder
	sb.WriteString(`
		select` + selectColumns + `
		from applications a
		join companies c on c.id = a.company_id`)

	writeWhere(&sb, filters(p, a, "a"))

	sb.WriteString(`
		order by a.applied_on desc, a.id
		limit ` + a.add(p.Limit) + ` offset ` + a.add(p.Offset))
	return sb.String()
}

// buildSearchList is the ?q= path: a union of two independently ranked
// branches, deduplicated by the higher rank.
//
// The branches are separate rather than OR'd into one WHERE for two reasons.
// First, index eligibility: OR-ing an unindexed ilike against the tsvector
// predicate forces the planner to scan every row to evaluate the OR, which
// defeats applications_search_idx. Second, ranking: a row that matches only on
// company name has ts_rank 0, so under a single ORDER BY ts_rank it sorts last
// however relevant it is. Giving the company branch a fixed 1.0 -- well above
// typical ts_rank values of 0.01 to 0.1 -- makes "Visa" return applications
// *at* Visa ahead of ones that merely mention Visa in the notes.
func buildSearchList(p ListParams, a *args) string {
	// Both branches bind the same two arguments, so they are added once here
	// and their placeholders reused below.
	tsquery := a.add(p.Query)
	pattern := a.add("%" + escapeLike(p.Query) + "%")

	textBranch := []string{
		"a.search_vec @@ plainto_tsquery('english', " + tsquery + ")",
	}
	companyBranch := []string{
		"c.name ilike " + pattern,
	}

	// Ownership and status apply inside each branch rather than outside the
	// union: filtering after the union would rank and collect rows the caller
	// is not allowed to see, then discard them, which is both slower and a
	// worse place for an access-control rule to live.
	shared := filters(p, a, "a")
	textBranch = append(textBranch, shared...)
	companyBranch = append(companyBranch, shared...)

	var sb strings.Builder
	sb.WriteString(`
		with matches as (
			select a.id, ts_rank(a.search_vec, plainto_tsquery('english', ` + tsquery + `)) as rank
			from applications a
			where ` + strings.Join(textBranch, "\n			  and ") + `

			union all

			select a.id, 1.0 as rank
			from applications a
			join companies c on c.id = a.company_id
			where ` + strings.Join(companyBranch, "\n			  and ") + `
		),
		ranked as (
			select id, max(rank) as rank from matches group by id
		)
		select` + selectColumns + `
		from ranked r
		join applications a on a.id = r.id
		join companies c on c.id = a.company_id
		order by r.rank desc, a.applied_on desc, a.id
		limit ` + a.add(p.Limit) + ` offset ` + a.add(p.Offset))
	return sb.String()
}

// filters returns the ownership and status conditions common to every read.
func filters(p ListParams, a *args, alias string) []string {
	var conds []string
	if p.OwnerID != nil {
		conds = append(conds, alias+".user_id = "+a.add(*p.OwnerID))
	}
	if p.Status != "" {
		conds = append(conds, alias+".status = "+a.add(string(p.Status)))
	}
	return conds
}

func writeWhere(sb *strings.Builder, conds []string) {
	if len(conds) == 0 {
		return
	}
	sb.WriteString("\n\t\twhere " + strings.Join(conds, "\n\t\t  and "))
}

// escapeLike neutralises the wildcards in a user-supplied ilike pattern.
//
// The value is already a bind parameter, so this is not about injection --
// it is about meaning. Without it, searching "100%" matches every company and
// "a_b" matches "axb", which makes the search look broken rather than
// permissive. Backslash is Postgres's default LIKE escape character, so
// escaping it first keeps a literal backslash literal.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
