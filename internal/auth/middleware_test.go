package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
)

// spyHandler records whether the middleware let a request through and what
// principal it saw, which is the half of the contract a status code cannot
// show.
type spyHandler struct {
	called    bool
	principal auth.Principal
	found     bool
}

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	s.principal, s.found = auth.PrincipalFrom(r.Context())
	w.WriteHeader(http.StatusOK)
}

// stubVerifier stands in for *auth.Issuer so middleware tests do not pay for
// token signing, and so a verifier failure can be forced exactly.
type stubVerifier struct {
	principal auth.Principal
	err       error
	sawToken  string
}

func (s *stubVerifier) Verify(token string) (auth.Principal, error) {
	s.sawToken = token
	return s.principal, s.err
}

func TestRequireAuth_RejectsEveryBadHeaderWithOne401(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"basic scheme", "Basic dXNlcjpwYXNz"},
		{"scheme only", "Bearer"},
		{"empty credential", "Bearer "},
		{"whitespace credential", "Bearer    "},
		{"token without scheme", "eyJhbGciOiJIUzI1NiJ9.e30."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &spyHandler{}
			// A verifier that would accept anything, to prove the header itself
			// is what got rejected.
			verifier := &stubVerifier{principal: auth.Principal{ID: uuid.New(), Role: auth.RoleAdmin}}

			rec := serve(t, auth.RequireAuth(verifier)(next), tt.header)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
			assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"), "RFC 7235 requires a challenge on 401")
			assert.False(t, next.called, "the handler must never run")
		})
	}
}

func TestRequireAuth_RejectsWhenVerifierFails(t *testing.T) {
	next := &spyHandler{}
	verifier := &stubVerifier{err: auth.ErrInvalidToken}

	rec := serve(t, auth.RequireAuth(verifier)(next), "Bearer some.jwt.value")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.False(t, next.called)
	// Whatever went wrong, the body must not say. "expired" versus "bad
	// signature" is free information for someone probing captured tokens.
	assert.NotContains(t, rec.Body.String(), "expired")
	assert.NotContains(t, rec.Body.String(), "invalid token")
}

func TestRequireAuth_PassesAndInjectsPrincipal(t *testing.T) {
	id := uuid.New()
	next := &spyHandler{}
	verifier := &stubVerifier{principal: auth.Principal{ID: id, Role: auth.RoleUser}}

	rec := serve(t, auth.RequireAuth(verifier)(next), "Bearer the.real.token")

	assert.Equal(t, http.StatusOK, rec.Code)
	require.True(t, next.called)
	require.True(t, next.found, "handlers downstream must find a principal")
	assert.Equal(t, id, next.principal.ID)
	assert.Equal(t, auth.RoleUser, next.principal.Role)
	assert.Equal(t, "the.real.token", verifier.sawToken, "the credential is passed through unmodified")
}

// RFC 7235 defines the scheme as case-insensitive; a hand-rolled client
// sending "bearer" is not an attacker and should not get a 401 nobody can
// explain.
func TestRequireAuth_SchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			next := &spyHandler{}
			verifier := &stubVerifier{principal: auth.Principal{ID: uuid.New(), Role: auth.RoleUser}}

			rec := serve(t, auth.RequireAuth(verifier)(next), scheme+" token")

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.True(t, next.called)
		})
	}
}

// End to end against the real issuer, so the middleware is proven against the
// tokens it will actually receive rather than only against a stub.
func TestRequireAuth_WithRealIssuer(t *testing.T) {
	issuer := newIssuer(t)
	id := uuid.New()
	token, err := issuer.Issue(id, auth.RoleAdmin)
	require.NoError(t, err)

	next := &spyHandler{}
	rec := serve(t, auth.RequireAuth(issuer)(next), "Bearer "+token)

	assert.Equal(t, http.StatusOK, rec.Code)
	require.True(t, next.found)
	assert.Equal(t, id, next.principal.ID)
	assert.Equal(t, auth.RoleAdmin, next.principal.Role)

	// And the forgery is refused through the full stack, not just at Verify.
	forged := algNoneToken(t, defaultClaims(id, auth.RoleAdmin))
	blocked := &spyHandler{}
	rec = serve(t, auth.RequireAuth(issuer)(blocked), "Bearer "+forged)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, blocked.called)
}

func TestRequireRole(t *testing.T) {
	tests := []struct {
		name      string
		principal *auth.Principal
		want      int
		wantBody  string
	}{
		{"admin passes", &auth.Principal{ID: uuid.New(), Role: auth.RoleAdmin}, http.StatusOK, ""},
		{"user forbidden", &auth.Principal{ID: uuid.New(), Role: auth.RoleUser}, http.StatusForbidden, `{"error":"forbidden"}`},
		{"unknown role forbidden", &auth.Principal{ID: uuid.New(), Role: "superuser"}, http.StatusForbidden, `{"error":"forbidden"}`},
		// No principal means the route was mounted without RequireAuth. That is
		// a wiring bug, and 401 is the honest answer: nobody was identified.
		{"no principal is 401", nil, http.StatusUnauthorized, `{"error":"unauthorized"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &spyHandler{}
			req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
			if tt.principal != nil {
				req = req.WithContext(auth.WithPrincipal(req.Context(), *tt.principal))
			}

			rec := httptest.NewRecorder()
			auth.RequireRole(auth.RoleAdmin)(next).ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.want == http.StatusOK, next.called)
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, rec.Body.String())
			}
		})
	}
}

// The two middlewares are always mounted as a pair; this proves the pair works
// in the order the router will use.
func TestRequireAuth_ThenRequireRole(t *testing.T) {
	issuer := newIssuer(t)

	for _, tc := range []struct {
		role string
		want int
	}{
		{auth.RoleAdmin, http.StatusOK},
		{auth.RoleUser, http.StatusForbidden},
	} {
		t.Run(tc.role, func(t *testing.T) {
			token, err := issuer.Issue(uuid.New(), tc.role)
			require.NoError(t, err)

			next := &spyHandler{}
			stack := auth.RequireAuth(issuer)(auth.RequireRole(auth.RoleAdmin)(next))

			rec := serve(t, stack, "Bearer "+token)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestPrincipalFrom_EmptyContext(t *testing.T) {
	_, ok := auth.PrincipalFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	assert.False(t, ok, "an unauthenticated request must not yield a zero-value principal that looks real")
}

func TestPrincipal_IsAdmin(t *testing.T) {
	assert.True(t, auth.Principal{Role: auth.RoleAdmin}.IsAdmin())
	assert.False(t, auth.Principal{Role: auth.RoleUser}.IsAdmin())
	assert.False(t, auth.Principal{}.IsAdmin(), "a zero principal is not an admin")
}

func TestValidRole(t *testing.T) {
	assert.True(t, auth.ValidRole(auth.RoleUser))
	assert.True(t, auth.ValidRole(auth.RoleAdmin))
	assert.False(t, auth.ValidRole("superuser"))
	assert.False(t, auth.ValidRole(""))
	assert.False(t, auth.ValidRole("Admin"), "roles are compared exactly; the check constraint is lowercase")
}

// serve runs h against a GET carrying the given Authorization header.
func serve(t *testing.T, h http.Handler, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/applications", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
