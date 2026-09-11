package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/server"
)

const testSecret = "router-test-secret-at-least-32-bytes-long"

// newRouter builds the real router with a nil pool.
//
// That is safe for everything below because none of these requests reach a
// handler that touches the database: the protected routes are rejected by
// RequireAuth first, and the public ones fail at body decoding.
func newRouter(t *testing.T) http.Handler {
	t.Helper()
	issuer, err := auth.NewIssuer(testSecret, auth.DefaultTTL)
	require.NoError(t, err)
	return server.New(nil, issuer)
}

// protectedRoutes is every route that must require a token. Adding a route to
// the router without adding it here is fine; adding one here that the router
// leaves unauthenticated fails.
var protectedRoutes = []struct{ method, path string }{
	{http.MethodGet, "/applications"},
	{http.MethodPost, "/applications"},
	{http.MethodGet, "/applications/" + uuid.Nil.String()},
	{http.MethodPatch, "/applications/" + uuid.Nil.String()},
	{http.MethodDelete, "/applications/" + uuid.Nil.String()},
	{http.MethodPost, "/applications/" + uuid.Nil.String() + "/transition"},
	{http.MethodGet, "/applications/" + uuid.Nil.String() + "/history"},
	{http.MethodGet, "/admin/users"},
}

func TestRouter_EveryProtectedRouteRequiresAToken(t *testing.T) {
	router := newRouter(t)

	for _, route := range protectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())

			// The status alone does not prove the route is inside the auth
			// group: every handler also refuses a request with no principal,
			// so a route mounted outside it would still answer 401. Only
			// auth.unauthorized sets WWW-Authenticate, so this header is what
			// distinguishes "the middleware stopped it" from "it got through
			// to a handler that happened to defend itself".
			assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"),
				"route answered 401 but not from RequireAuth -- is it mounted outside the auth group?")
		})
	}
}

// A valid user token gets past RequireAuth but must still be refused by
// RequireRole on the admin group.
func TestRouter_AdminGroupRequiresTheAdminRole(t *testing.T) {
	issuer, err := auth.NewIssuer(testSecret, auth.DefaultTTL)
	require.NoError(t, err)
	router := server.New(nil, issuer)

	token, err := issuer.Issue(uuid.New(), auth.RoleUser)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":"forbidden"}`, rec.Body.String())
}

// The auth routes have to stay outside the token wall, or nobody can ever get
// a token. A 400 here means the request reached the handler and failed on its
// body, which is the proof that it was not rejected for being anonymous.
func TestRouter_AuthRoutesArePublic(t *testing.T) {
	router := newRouter(t)

	for _, path := range []string{"/auth/register", "/auth/login"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("")))

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.NotContains(t, rec.Body.String(), "unauthorized")
		})
	}
}

// PRD Endpoints says "All JSON". chi answers both of these in plain text by
// default, and they are the two a client is likeliest to hit by accident.
func TestRouter_UnknownRouteAndMethodAreJSON(t *testing.T) {
	router := newRouter(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"not found"}`, rec.Body.String())

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/auth/login", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.JSONEq(t, `{"error":"method not allowed"}`, rec.Body.String())
}

// The id is in every log line the server writes, so returning it turns "the
// API gave me a 500" into a grep.
func TestRouter_EchoesRequestID(t *testing.T) {
	router := newRouter(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/applications", nil))

	assert.NotEmpty(t, rec.Header().Get(httpx.RequestIDHeader),
		"middleware.RequestID must run before EchoRequestID")
}
