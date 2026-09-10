package auth

import (
	"net/http"
	"strings"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
)

// Verifier is the half of Issuer that RequireAuth needs. Taking the interface
// rather than *Issuer keeps the middleware testable with a stub and documents
// that it never mints anything.
type Verifier interface {
	Verify(token string) (Principal, error)
}

// RequireAuth rejects any request without a valid bearer token and puts the
// principal on the context for handlers downstream.
//
// Every rejection is the same 401 with the same body. Distinguishing "no
// header" from "expired" from "bad signature" would let an attacker sort
// captured tokens into interesting and uninteresting without ever landing a
// request.
func RequireAuth(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w)
				return
			}

			principal, err := v.Verify(token)
			if err != nil {
				unauthorized(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// RequireRole rejects an authenticated caller who does not hold role.
//
// It answers 401, not 403, when there is no principal at all. That case means
// the route was mounted without RequireAuth in front -- a wiring bug -- and
// 403 would imply the caller was identified and found wanting, which is a
// misleading thing to tell them and a misleading thing to read in a log.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := PrincipalFrom(r.Context())
			if !ok {
				unauthorized(w)
				return
			}
			if principal.Role != role {
				httpx.WriteError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken pulls the credential out of an Authorization header.
//
// The scheme match is case-insensitive because RFC 7235 §2.1 defines it that
// way -- "bearer" from a hand-rolled client is as valid as "Bearer" from a
// library, and rejecting it produces a 401 nobody can debug.
func bearerToken(header string) (string, bool) {
	scheme, credentials, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(credentials)
	if token == "" {
		return "", false
	}
	return token, true
}

// unauthorized writes the single 401 every auth failure shares. The
// WWW-Authenticate header is required by RFC 7235 §3.1 on a 401 and tells a
// client which scheme to retry with.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
}
