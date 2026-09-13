//go:build integration

// Package integration drives the real router against a real Postgres.
//
// It builds the handler with server.New -- the same call cmd/api/main.go makes
// -- so route wiring, middleware order and the ownership rules are all
// exercised as they ship rather than as a test file re-declares them.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/applications"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/db"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/dbtest"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/server"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/users"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

const (
	jwtSecret    = "integration-test-secret-at-least-32-bytes"
	testPassword = "correct-horse-battery-staple"
)

var (
	baseURL string
	pool    *pgxpool.Pool
)

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runTests migrates a clean schema and stands the server up once for the whole
// package. One server and one pool, because the cost worth avoiding is the
// per-test connection setup, not the truncate.
func runTests(m *testing.M) (int, error) {
	ctx := context.Background()

	// Deliberately not the database DATABASE_URL names, and deliberately not
	// one any other suite uses. This suite drops the public schema, so it runs
	// against a database it owns outright -- otherwise a run deletes whatever
	// the developer who started it was working on, or races the internal/db
	// suite that `go test ./...` is running in parallel with it.
	tdb, err := dbtest.Open(ctx, dbtest.SuiteAPI)
	if err != nil {
		return 0, err
	}
	if err := tdb.Reset(ctx); err != nil {
		return 0, err
	}
	if err := db.Up(tdb.URL); err != nil {
		return 0, err
	}

	pool, err = db.New(ctx, tdb.URL)
	if err != nil {
		return 0, err
	}
	defer pool.Close()

	issuer, err := auth.NewIssuer(jwtSecret, auth.DefaultTTL)
	if err != nil {
		return 0, err
	}

	srv := httptest.NewServer(server.New(pool, issuer))
	defer srv.Close()
	baseURL = srv.URL

	return m.Run(), nil
}

// fresh empties every table. It runs at the start of a test rather than in
// t.Cleanup so a failure leaves its rows behind to look at, while the next
// test still starts from nothing either way.
//
// No test calls t.Parallel: they share one database.
func fresh(t *testing.T) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`truncate users, companies, applications, status_transitions restart identity cascade`)
	require.NoError(t, err)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// into decodes a JSON response body, reporting the raw body on failure --
// an unexpected error envelope is far more useful than "cannot unmarshal
// object into Go value of type ...".
func (r response) into(t *testing.T, dst any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, dst), "body: %s", r.body)
}

func (r response) text() string { return string(r.body) }

// do issues one request. A string body is sent verbatim, so a test can send
// malformed JSON; anything else is marshalled.
func do(t *testing.T, method, path, token string, body any) response {
	t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, baseURL+path, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	payload, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	return response{status: res.StatusCode, header: res.Header, body: payload}
}

func uniqueEmail() string {
	return "user-" + uuid.NewString()[:8] + "@example.com"
}

// registerUser creates a user-role account through the API and logs in,
// returning both the account and a usable token.
func registerUser(t *testing.T) (users.User, string) {
	t.Helper()

	email := uniqueEmail()
	res := do(t, http.MethodPost, "/auth/register", "", users.Credentials{Email: email, Password: testPassword})
	require.Equal(t, http.StatusCreated, res.status, res.text())

	var u users.User
	res.into(t, &u)
	return u, loginAs(t, email, testPassword)
}

// registerAdmin seeds an admin the only way production does, through
// users.EnsureAdmin. There is no HTTP route that mints one.
func registerAdmin(t *testing.T) (users.User, string) {
	t.Helper()

	email := uniqueEmail()
	admin, err := users.EnsureAdmin(context.Background(), users.NewRepo(pool), email, testPassword)
	require.NoError(t, err)
	require.Equal(t, auth.RoleAdmin, admin.Role)

	return admin, loginAs(t, email, testPassword)
}

func loginAs(t *testing.T, email, password string) string {
	t.Helper()

	res := do(t, http.MethodPost, "/auth/login", "", users.Credentials{Email: email, Password: password})
	require.Equal(t, http.StatusOK, res.status, res.text())

	var token users.TokenResponse
	res.into(t, &token)
	require.NotEmpty(t, token.Token)
	return token.Token
}

func createApp(t *testing.T, token string, in applications.CreateInput) applications.Application {
	t.Helper()

	res := do(t, http.MethodPost, "/applications", token, in)
	require.Equal(t, http.StatusCreated, res.status, res.text())

	var app applications.Application
	res.into(t, &app)
	return app
}

func listApps(t *testing.T, token, query string) []applications.Application {
	t.Helper()

	res := do(t, http.MethodGet, "/applications"+query, token, nil)
	require.Equal(t, http.StatusOK, res.status, res.text())

	var found []applications.Application
	res.into(t, &found)
	return found
}

func transition(t *testing.T, token string, id uuid.UUID, to workflow.State) response {
	t.Helper()
	return do(t, http.MethodPost, "/applications/"+id.String()+"/transition", token,
		applications.TransitionInput{To: to})
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func date(t *testing.T, s string) *applications.Date {
	t.Helper()

	parsed, err := time.Parse(applications.DateLayout, s)
	require.NoError(t, err)
	d := applications.Date(parsed)
	return &d
}

func ptr[T any](v T) *T { return &v }

// ids maps applications to their ids, so an assertion can name the rows it
// expects instead of indexing into a slice.
func ids(apps []applications.Application) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.ID)
	}
	return out
}
