//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/applications"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/users"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// 1. Health check.
func TestHealthz(t *testing.T) {
	res := do(t, http.MethodGet, "/healthz", "", nil)

	assert.Equal(t, http.StatusOK, res.status)
	assert.JSONEq(t, `{"status":"ok"}`, res.text())
}

// 2. Registration.
func TestRegister(t *testing.T) {
	fresh(t)

	email := uniqueEmail()
	res := do(t, http.MethodPost, "/auth/register", "", users.Credentials{Email: email, Password: testPassword})

	require.Equal(t, http.StatusCreated, res.status, res.text())
	var created users.User
	res.into(t, &created)
	assert.Equal(t, email, created.Email)
	assert.Equal(t, auth.RoleUser, created.Role)
	assert.NotEqual(t, uuid.Nil, created.ID)

	// The json:"-" tag on PasswordHash is the only thing keeping the hash out
	// of this body, so check the wire bytes rather than the decoded struct.
	assert.NotContains(t, res.text(), "password_hash")
	assert.NotContains(t, res.text(), "$2a$")

	t.Run("duplicate email is 409", func(t *testing.T) {
		again := do(t, http.MethodPost, "/auth/register", "", users.Credentials{Email: email, Password: testPassword})
		assert.Equal(t, http.StatusConflict, again.status)
		assert.JSONEq(t, `{"error":"email already registered"}`, again.text())
	})

	t.Run("case is normalized, so a shouted duplicate is still a duplicate", func(t *testing.T) {
		again := do(t, http.MethodPost, "/auth/register", "",
			users.Credentials{Email: strings.ToUpper(email), Password: testPassword})
		assert.Equal(t, http.StatusConflict, again.status)
	})

	for name, body := range map[string]any{
		"not an address":     users.Credentials{Email: "not-an-email", Password: testPassword},
		"display-name form":  users.Credentials{Email: `"Aaron" <aaron@example.com>`, Password: testPassword},
		"password too short": users.Credentials{Email: uniqueEmail(), Password: "short"},
		"password over bcrypt's 72-byte limit": users.Credentials{
			Email: uniqueEmail(), Password: strings.Repeat("x", 73),
		},
		"malformed json": `{"email":`,
	} {
		t.Run(name+" is 400", func(t *testing.T) {
			bad := do(t, http.MethodPost, "/auth/register", "", body)
			assert.Equal(t, http.StatusBadRequest, bad.status, bad.text())
		})
	}
}

// 3. Login.
func TestLogin(t *testing.T) {
	fresh(t)

	account, token := registerUser(t)

	// The token works on a protected route.
	assert.Equal(t, http.StatusOK, do(t, http.MethodGet, "/applications", token, nil).status)

	// Normalization applies at login too, or an account registered lowercase
	// would be unreachable from a client that title-cases the address.
	assert.NotEmpty(t, loginAs(t, strings.ToUpper(account.Email), testPassword))

	wrongPassword := do(t, http.MethodPost, "/auth/login", "",
		users.Credentials{Email: account.Email, Password: "not-the-password"})
	unknownEmail := do(t, http.MethodPost, "/auth/login", "",
		users.Credentials{Email: uniqueEmail(), Password: "not-the-password"})
	malformedEmail := do(t, http.MethodPost, "/auth/login", "",
		users.Credentials{Email: "not-an-email", Password: "not-the-password"})

	assert.Equal(t, http.StatusUnauthorized, wrongPassword.status)
	assert.JSONEq(t, `{"error":"invalid email or password"}`, wrongPassword.text())

	// Byte-identical, all three. A 400 for the malformed address would confirm
	// that well-formed ones get further, and a different body for an unknown
	// email would answer "is this address registered?".
	assert.Equal(t, wrongPassword.status, unknownEmail.status)
	assert.Equal(t, wrongPassword.text(), unknownEmail.text())
	assert.Equal(t, wrongPassword.status, malformedEmail.status)
	assert.Equal(t, wrongPassword.text(), malformedEmail.text())
}

// 4. Creating two applications at one company must not create two companies.
func TestCreate_UpsertsTheCompany(t *testing.T) {
	fresh(t)
	_, token := registerUser(t)

	first := createApp(t, token, applications.CreateInput{
		CompanyName: "Acme Corp", RoleTitle: "Backend Engineer", AppliedOn: date(t, "2026-02-14"),
	})
	second := createApp(t, token, applications.CreateInput{
		CompanyName: "Acme Corp", RoleTitle: "Platform Engineer",
	})

	assert.Equal(t, first.CompanyID, second.CompanyID)
	assert.Equal(t, "Acme Corp", second.CompanyName)
	assert.Equal(t, 1, countRows(t, `select count(*) from companies`))

	// applied_on is a date column, so it goes out as YYYY-MM-DD rather than an
	// RFC3339 timestamp with an invented time and zone.
	assert.Equal(t, "2026-02-14", first.AppliedOn.String())
	assert.Equal(t, workflow.StateApplied, first.Status)

	t.Run("the name is trimmed before the unique index sees it", func(t *testing.T) {
		padded := createApp(t, token, applications.CreateInput{
			CompanyName: "  Acme Corp  ", RoleTitle: "Product Manager",
		})
		assert.Equal(t, first.CompanyID, padded.CompanyID)
		assert.Equal(t, 1, countRows(t, `select count(*) from companies`))
	})

	t.Run("company_name is required", func(t *testing.T) {
		res := do(t, http.MethodPost, "/applications", token,
			applications.CreateInput{RoleTitle: "Engineer"})
		assert.Equal(t, http.StatusBadRequest, res.status)
	})
}

// 5. Ownership on reads.
func TestRead_OwnershipIsA404(t *testing.T) {
	fresh(t)
	_, ownerToken := registerUser(t)
	_, strangerToken := registerUser(t)

	app := createApp(t, ownerToken, applications.CreateInput{
		CompanyName: "Initech", RoleTitle: "Engineer",
	})
	path := "/applications/" + app.ID.String()

	assert.Equal(t, http.StatusOK, do(t, http.MethodGet, path, ownerToken, nil).status)

	for _, route := range []string{path, path + "/history"} {
		stranger := do(t, http.MethodGet, route, strangerToken, nil)

		// 404, not 403: a 403 would confirm the id exists and belongs to
		// somebody, which is exactly what a probe is looking for.
		assert.Equal(t, http.StatusNotFound, stranger.status, route)
		assert.JSONEq(t, `{"error":"application not found"}`, stranger.text())

		assert.Equal(t, http.StatusUnauthorized, do(t, http.MethodGet, route, "", nil).status, route)
	}

	t.Run("an id that is not a uuid is also 404", func(t *testing.T) {
		res := do(t, http.MethodGet, "/applications/not-a-uuid", ownerToken, nil)
		assert.Equal(t, http.StatusNotFound, res.status)
		assert.JSONEq(t, `{"error":"application not found"}`, res.text())
	})

	t.Run("a well-formed id nobody owns is 404", func(t *testing.T) {
		res := do(t, http.MethodGet, "/applications/"+uuid.NewString(), ownerToken, nil)
		assert.Equal(t, http.StatusNotFound, res.status)
	})

	t.Run("the stranger's own list does not include it", func(t *testing.T) {
		assert.Empty(t, listApps(t, strangerToken, ""))
	})
}

// 6. Transitions, the audit trail, and the two distinct failure modes.
func TestTransition(t *testing.T) {
	fresh(t)
	owner, token := registerUser(t)

	app := createApp(t, token, applications.CreateInput{
		CompanyName: "Globex", RoleTitle: "Engineer",
	})
	path := "/applications/" + app.ID.String()

	res := transition(t, token, app.ID, workflow.StateScreening)
	require.Equal(t, http.StatusOK, res.status, res.text())
	var moved applications.Application
	res.into(t, &moved)
	assert.Equal(t, workflow.StateScreening, moved.Status)
	assert.True(t, moved.UpdatedAt.After(app.UpdatedAt))

	var history []applications.Transition
	do(t, http.MethodGet, path+"/history", token, nil).into(t, &history)
	require.Len(t, history, 1)
	assert.Equal(t, workflow.StateApplied, history[0].FromStatus)
	assert.Equal(t, workflow.StateScreening, history[0].ToStatus)
	assert.Equal(t, owner.ID, history[0].ChangedBy)

	t.Run("going backwards is 409 and says what would be legal", func(t *testing.T) {
		res := transition(t, token, app.ID, workflow.StateApplied)
		require.Equal(t, http.StatusConflict, res.status, res.text())

		var body applications.InvalidTransitionBody
		res.into(t, &body)
		assert.Equal(t, "invalid transition", body.Error)
		assert.Equal(t, workflow.StateScreening, body.From)
		assert.Equal(t, workflow.Next(workflow.StateScreening), body.Allowed)
	})

	t.Run("a target that is not a state at all is 400, not 409", func(t *testing.T) {
		res := do(t, http.MethodPost, path+"/transition", token, `{"to":"bogus"}`)
		assert.Equal(t, http.StatusBadRequest, res.status)
		assert.JSONEq(t, `{"error":"unknown state"}`, res.text())
	})

	t.Run("a terminal state reports allowed as [] rather than null", func(t *testing.T) {
		require.Equal(t, http.StatusOK, transition(t, token, app.ID, workflow.StateRejected).status)

		res := transition(t, token, app.ID, workflow.StateScreening)
		require.Equal(t, http.StatusConflict, res.status)

		var body applications.InvalidTransitionBody
		res.into(t, &body)
		assert.Equal(t, workflow.StateRejected, body.From)
		assert.Empty(t, body.Allowed)
		// A client doing `for (const s of body.allowed)` breaks on null.
		assert.Contains(t, res.text(), `"allowed":[]`)
	})

	t.Run("rejected moves leave no audit rows behind", func(t *testing.T) {
		// Two successful moves, four attempts. The failures are checked before
		// the transaction writes anything.
		assert.Equal(t, 2, countRows(t,
			`select count(*) from status_transitions where application_id = $1`, app.ID))
	})

	t.Run("a stranger cannot transition it", func(t *testing.T) {
		_, strangerToken := registerUser(t)
		assert.Equal(t, http.StatusNotFound, transition(t, strangerToken, app.ID, workflow.StateWithdrawn).status)
	})
}

// 7. PATCH semantics, including absent-vs-null and the status trap.
func TestPatch(t *testing.T) {
	fresh(t)
	_, token := registerUser(t)

	app := createApp(t, token, applications.CreateInput{
		CompanyName: "Hooli", RoleTitle: "Engineer", Notes: ptr("first pass"),
	})
	path := "/applications/" + app.ID.String()

	res := do(t, http.MethodPatch, path, token, `{"role_title":"Staff Engineer"}`)
	require.Equal(t, http.StatusOK, res.status, res.text())
	var updated applications.Application
	res.into(t, &updated)
	assert.Equal(t, "Staff Engineer", updated.RoleTitle)
	assert.True(t, updated.UpdatedAt.After(app.UpdatedAt), "updated_at must be bumped by the UPDATE")
	assert.Equal(t, "Hooli", updated.CompanyName)
	assert.Equal(t, ptr("first pass"), updated.Notes, "an absent key must leave the column alone")

	t.Run("an explicit null clears a nullable column", func(t *testing.T) {
		res := do(t, http.MethodPatch, path, token, `{"notes":null}`)
		require.Equal(t, http.StatusOK, res.status, res.text())
		res.into(t, &updated)
		assert.Nil(t, updated.Notes)
	})

	t.Run("and a later patch of a different field leaves it cleared", func(t *testing.T) {
		res := do(t, http.MethodPatch, path, token, `{"location":"Remote"}`)
		require.Equal(t, http.StatusOK, res.status, res.text())
		res.into(t, &updated)
		assert.Equal(t, ptr("Remote"), updated.Location)
		assert.Nil(t, updated.Notes)
	})

	t.Run("status in the body is 400 and names the right endpoint", func(t *testing.T) {
		res := do(t, http.MethodPatch, path, token, `{"status":"offer"}`)
		assert.Equal(t, http.StatusBadRequest, res.status)
		assert.Contains(t, res.text(), "use POST /applications/{id}/transition")

		// And it really did not change.
		var after applications.Application
		do(t, http.MethodGet, path, token, nil).into(t, &after)
		assert.Equal(t, workflow.StateApplied, after.Status)
	})

	for name, body := range map[string]string{
		"an empty patch":             `{}`,
		"an unknown field":           `{"nope":1}`,
		"a null role_title":          `{"role_title":null}`,
		"an empty role_title":        `{"role_title":"   "}`,
		"a timestamp for applied_on": `{"applied_on":"2026-02-14T00:00:00Z"}`,
	} {
		t.Run(name+" is 400", func(t *testing.T) {
			assert.Equal(t, http.StatusBadRequest, do(t, http.MethodPatch, path, token, body).status)
		})
	}

	t.Run("a stranger cannot patch it", func(t *testing.T) {
		_, strangerToken := registerUser(t)
		res := do(t, http.MethodPatch, path, strangerToken, `{"role_title":"Hijacked"}`)
		assert.Equal(t, http.StatusNotFound, res.status)
	})
}

// 8. Search across both branches of the union.
func TestSearch(t *testing.T) {
	fresh(t)
	_, token := registerUser(t)

	atVisa := createApp(t, token, applications.CreateInput{
		CompanyName: "Visa", RoleTitle: "Backend Engineer", AppliedOn: date(t, "2026-03-01"),
	})
	mentionsVisa := createApp(t, token, applications.CreateInput{
		CompanyName: "Acme", RoleTitle: "Platform Engineer",
		Notes: ptr("we process payments through Visa"), AppliedOn: date(t, "2026-03-02"),
	})
	unrelated := createApp(t, token, applications.CreateInput{
		CompanyName: "Globex", RoleTitle: "Designer", AppliedOn: date(t, "2026-03-03"),
	})

	t.Run("the company branch outranks an incidental mention", func(t *testing.T) {
		found := listApps(t, token, "?q=Visa")

		// The whole reason for the fixed 1.0 rank on the company branch: under
		// the PRD's single `order by ts_rank`, the company match scores 0 and
		// sorts last, so searching "Visa" would not surface the application
		// actually at Visa first.
		require.Equal(t, []uuid.UUID{atVisa.ID, mentionsVisa.ID}, ids(found))
		assert.NotContains(t, ids(found), unrelated.ID)
	})

	t.Run("the tsvector branch matches notes", func(t *testing.T) {
		assert.Equal(t, []uuid.UUID{mentionsVisa.ID}, ids(listApps(t, token, "?q=payments")))
	})

	t.Run("and role_title, stemmed", func(t *testing.T) {
		// plainto_tsquery('english', 'designing') stems to 'design', which is
		// what to_tsvector stored for "Designer".
		assert.Equal(t, []uuid.UUID{unrelated.ID}, ids(listApps(t, token, "?q=designing")))
	})

	t.Run("no match is an empty array, not null", func(t *testing.T) {
		res := do(t, http.MethodGet, "/applications?q=nothingmatchesthis", token, nil)
		require.Equal(t, http.StatusOK, res.status)
		assert.Equal(t, "[]", strings.TrimSpace(res.text()))
	})

	t.Run("a wildcard in the term is a literal, not a wildcard", func(t *testing.T) {
		// escapeLike is why: without it '%' reaches the ilike unescaped and
		// matches every company on file.
		assert.Empty(t, listApps(t, token, "?q=%25"))
	})

	t.Run("search is still scoped to the caller", func(t *testing.T) {
		_, strangerToken := registerUser(t)
		assert.Empty(t, listApps(t, strangerToken, "?q=Visa"))
	})
}

// 9. Filtering and pagination.
func TestListFiltersAndPaging(t *testing.T) {
	fresh(t)
	_, token := registerUser(t)

	// Distinct dates, because the order is applied_on desc.
	newest := createApp(t, token, applications.CreateInput{
		CompanyName: "Acme", RoleTitle: "Engineer", AppliedOn: date(t, "2026-01-03"),
	})
	middle := createApp(t, token, applications.CreateInput{
		CompanyName: "Initech", RoleTitle: "Engineer", AppliedOn: date(t, "2026-01-02"),
	})
	oldest := createApp(t, token, applications.CreateInput{
		CompanyName: "Globex", RoleTitle: "Engineer", AppliedOn: date(t, "2026-01-01"),
	})

	assert.Equal(t, []uuid.UUID{newest.ID, middle.ID, oldest.ID}, ids(listApps(t, token, "")))

	require.Equal(t, http.StatusOK, transition(t, token, newest.ID, workflow.StateScreening).status)

	assert.Equal(t, []uuid.UUID{newest.ID}, ids(listApps(t, token, "?status=screening")))
	assert.Equal(t, []uuid.UUID{middle.ID, oldest.ID}, ids(listApps(t, token, "?status=applied")))

	assert.Equal(t, []uuid.UUID{newest.ID, middle.ID}, ids(listApps(t, token, "?limit=2")))
	assert.Equal(t, []uuid.UUID{oldest.ID}, ids(listApps(t, token, "?offset=2")))
	assert.Equal(t, []uuid.UUID{middle.ID}, ids(listApps(t, token, "?limit=1&offset=1")))

	t.Run("status and q are ANDed", func(t *testing.T) {
		assert.Empty(t, ids(listApps(t, token, "?q=Engineer&status=offer")))
		assert.Equal(t, []uuid.UUID{newest.ID}, ids(listApps(t, token, "?q=Engineer&status=screening")))
	})

	t.Run("an over-max limit is clamped, not rejected", func(t *testing.T) {
		res := do(t, http.MethodGet, "/applications?limit=5000", token, nil)
		assert.Equal(t, http.StatusOK, res.status)
	})

	for name, query := range map[string]string{
		"unknown status":    "?status=bogus",
		"negative limit":    "?limit=-1",
		"negative offset":   "?offset=-1",
		"non-numeric limit": "?limit=lots",
	} {
		t.Run(name+" is 400", func(t *testing.T) {
			assert.Equal(t, http.StatusBadRequest, do(t, http.MethodGet, "/applications"+query, token, nil).status)
		})
	}
}

// 10. Delete, and the cascade to the audit trail.
func TestDelete(t *testing.T) {
	fresh(t)
	_, token := registerUser(t)

	app := createApp(t, token, applications.CreateInput{CompanyName: "Umbrella", RoleTitle: "Engineer"})
	path := "/applications/" + app.ID.String()

	require.Equal(t, http.StatusOK, transition(t, token, app.ID, workflow.StateScreening).status)
	require.Equal(t, 1, countRows(t, `select count(*) from status_transitions where application_id = $1`, app.ID))

	t.Run("a stranger cannot delete it", func(t *testing.T) {
		_, strangerToken := registerUser(t)
		assert.Equal(t, http.StatusNotFound, do(t, http.MethodDelete, path, strangerToken, nil).status)
		assert.Equal(t, 1, countRows(t, `select count(*) from applications where id = $1`, app.ID))
	})

	res := do(t, http.MethodDelete, path, token, nil)
	assert.Equal(t, http.StatusNoContent, res.status)
	assert.Empty(t, res.body)

	assert.Equal(t, http.StatusNotFound, do(t, http.MethodGet, path, token, nil).status)
	assert.Equal(t, http.StatusNotFound, do(t, http.MethodDelete, path, token, nil).status)

	// on delete cascade on status_transitions.application_id.
	assert.Equal(t, 0, countRows(t, `select count(*) from status_transitions where application_id = $1`, app.ID))

	// The company outlives the application: it is shared, not owned.
	assert.Equal(t, 1, countRows(t, `select count(*) from companies`))
}

// 11. Admin: reads everything, writes nothing it does not own.
func TestAdmin(t *testing.T) {
	fresh(t)
	owner, ownerToken := registerUser(t)
	admin, adminToken := registerAdmin(t)

	app := createApp(t, ownerToken, applications.CreateInput{CompanyName: "Soylent", RoleTitle: "Engineer"})
	path := "/applications/" + app.ID.String()

	t.Run("reads every user's applications", func(t *testing.T) {
		assert.Contains(t, ids(listApps(t, adminToken, "")), app.ID)
		assert.Equal(t, http.StatusOK, do(t, http.MethodGet, path, adminToken, nil).status)
		assert.Equal(t, http.StatusOK, do(t, http.MethodGet, path+"/history", adminToken, nil).status)
	})

	// Admin is read-only on other people's rows, so a write gets the same 404
	// any other non-owner gets. The rule lives in the WHERE clause, not in a
	// role check, which is why there is nothing to forget to apply per route.
	t.Run("but cannot write them", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound,
			do(t, http.MethodPatch, path, adminToken, `{"role_title":"Reassigned"}`).status)
		assert.Equal(t, http.StatusNotFound,
			transition(t, adminToken, app.ID, workflow.StateScreening).status)
		assert.Equal(t, http.StatusNotFound,
			do(t, http.MethodDelete, path, adminToken, nil).status)

		var after applications.Application
		do(t, http.MethodGet, path, ownerToken, nil).into(t, &after)
		assert.Equal(t, "Engineer", after.RoleTitle)
		assert.Equal(t, workflow.StateApplied, after.Status)
	})

	t.Run("GET /admin/users lists accounts without their hashes", func(t *testing.T) {
		res := do(t, http.MethodGet, "/admin/users", adminToken, nil)
		require.Equal(t, http.StatusOK, res.status, res.text())

		var listed []users.User
		res.into(t, &listed)
		require.Len(t, listed, 2)
		assert.ElementsMatch(t, []string{owner.Email, admin.Email},
			[]string{listed[0].Email, listed[1].Email})

		assert.NotContains(t, res.text(), "password_hash")
		assert.NotContains(t, res.text(), "$2a$")
	})

	t.Run("a user-role token is 403 there", func(t *testing.T) {
		res := do(t, http.MethodGet, "/admin/users", ownerToken, nil)
		assert.Equal(t, http.StatusForbidden, res.status)
		assert.JSONEq(t, `{"error":"forbidden"}`, res.text())
	})

	t.Run("EnsureAdmin is idempotent and refreshes the password", func(t *testing.T) {
		again, err := users.EnsureAdmin(context.Background(), users.NewRepo(pool), admin.Email, "a-rotated-admin-password")
		require.NoError(t, err)
		assert.Equal(t, admin.ID, again.ID)
		assert.Equal(t, 2, countRows(t, `select count(*) from users`))
		assert.NotEmpty(t, loginAs(t, admin.Email, "a-rotated-admin-password"))
	})
}

// 12. The DoD's index claim, proved against the query that actually runs.
//
// Both shapes of the shipped ?q= query are checked: the owner-scoped one a
// user gets, and the unscoped one an admin gets. Whether the tsvector branch
// is *index-eligible* is what the union design decided; whether the planner
// then picks the index is a cost question, and the note on seedSearchCorpus
// records where that line falls.
func TestSearchQueryUsesTheGINIndex(t *testing.T) {
	fresh(t)
	owner, _ := registerUser(t)
	seedSearchCorpus(t, owner.ID)

	ownerID := owner.ID
	for name, scope := range map[string]*uuid.UUID{
		"owner-scoped": &ownerID,
		"admin":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			// The query under EXPLAIN is built by the same function the
			// handler calls, bind parameters and all. A hand-copied
			// approximation would prove the index works for a query nobody
			// runs.
			query, args := applications.BuildListQuery(applications.ListParams{
				Query:   searchTerm,
				Limit:   applications.DefaultLimit,
				OwnerID: scope,
			})

			plan := explain(t, query, args)
			t.Logf("query plan:\n%s", plan)

			assert.Contains(t, plan, "Bitmap Index Scan on applications_search_idx",
				"the tsvector branch must reach applications_search_idx; OR-ing the company "+
					"ilike into the same WHERE is what breaks this")

			// And the index is driven by the tsvector predicate itself, rather
			// than being reached incidentally by something else in the query.
			assert.Regexp(t, `Index Cond: \(.*search_vec @@`, plan)
		})
	}
}

// searchTerm appears in a handful of seeded rows. Selectivity is the whole
// point -- see seedSearchCorpus.
const searchTerm = "kubernetes"

// seedSearchCorpus fills applications with a corpus big enough for the planner
// to have a real choice to make.
//
// The term matters more than the row count. Postgres will not use
// applications_search_idx just because it exists: for an owner-scoped query it
// weighs a GIN scan against a plain btree scan of applications_user_idx with
// the tsvector predicate applied as a filter, and on this schema the btree wins
// whenever the search term is not selective. Measured on 2000 rows owned by one
// user: a term in 1 row in 10 plans as an Index Scan on applications_user_idx;
// a term in 1 row in 500 plans as a Bitmap Index Scan on
// applications_search_idx. That is the planner being right both times -- a
// term matching a tenth of the table is not what a GIN index is for -- so the
// corpus here uses a distinctive term, which is the case the index exists to
// serve.
func seedSearchCorpus(t *testing.T, owner uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var companyID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`insert into companies (name) values ('Seed Co') returning id`).Scan(&companyID))

	// This account exists only to own rows the owner cannot see, so that the
	// unscoped query really does cover more than the scoped one. Nothing logs
	// in as it, so the hash is a placeholder rather than a bcrypt round.
	var otherID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		insert into users (email, password_hash, role)
		values ('seed-other@example.com', 'not-a-real-hash', 'user')
		returning id`).Scan(&otherID))

	// Seeded in SQL rather than over HTTP: 2500 rows through the API would be
	// 2500 round trips to prove a planner claim.
	_, err := pool.Exec(ctx, `
		insert into applications (user_id, company_id, role_title, notes)
		select case when g.n <= 2000 then $1::uuid else $2::uuid end,
		       $3::uuid,
		       'Engineer ' || g.n,
		       case when g.n % 499 = 0 then 'kubernetes and postgres at scale'
		            else 'unremarkable notes ' || g.n end
		from generate_series(1, 2500) as g(n)`, owner, otherID, companyID)
	require.NoError(t, err)

	// VACUUM, not just ANALYZE, and the VACUUM is the part that matters.
	//
	// A GIN index buffers new entries in a pending list and only merges them
	// into the tree when it is vacuumed or the list fills. The planner prices
	// a scan over an unmerged list at what it would cost to read the list, so
	// straight after a bulk insert the same index on the same rows looks an
	// order of magnitude more expensive than it is: measured here, a GIN scan
	// costed at 127.56 before the vacuum and 12.84 after, which is the
	// difference between the planner rejecting applications_search_idx and
	// choosing it. Without this line the assertion below passes or fails
	// depending on whether autovacuum happened to run.
	_, err = pool.Exec(ctx, `vacuum analyze applications`)
	require.NoError(t, err)

	require.NotZero(t, countRows(t,
		`select count(*) from applications
		 where user_id = $1 and search_vec @@ plainto_tsquery('english', $2)`, owner, searchTerm),
		"the owner must match at least one row, or the scoped plan proves nothing")
}

// explain runs EXPLAIN over a parameterised query and returns the plan text.
func explain(t *testing.T, query string, args []any) string {
	t.Helper()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// A sequential scan can still win on cost at this size, and the planner is
	// right to choose it when it does. The claim being tested is that the GIN
	// index is reachable by this predicate, so seqscan is priced out of the
	// comparison rather than the table being grown until it loses.
	_, err = tx.Exec(ctx, `set local enable_seqscan = off`)
	require.NoError(t, err)

	rows, err := tx.Query(ctx, "explain "+query, args...)
	require.NoError(t, err)
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	return plan.String()
}
