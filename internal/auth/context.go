// Package auth issues and verifies HS256 JWTs, hashes passwords with bcrypt,
// and provides the two middlewares that guard the API: RequireAuth and
// RequireRole.
package auth

import (
	"context"

	"github.com/google/uuid"
)

// The two roles from the users.role check constraint.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Principal is the authenticated caller: everything the API knows about who is
// making a request, taken from the token and nothing else.
//
// It carries no email or password hash on purpose. Handlers that need more
// look the user up by ID; nothing downstream can accidentally serialise a
// secret it was handed for free.
type Principal struct {
	ID   uuid.UUID
	Role string
}

// IsAdmin reports whether the caller holds the admin role.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// ValidRole reports whether role is one the schema allows. Verify checks this
// so a token minted before a role was renamed cannot smuggle an unknown role
// past a comparison that only ever tests for "admin".
func ValidRole(role string) bool { return role == RoleUser || role == RoleAdmin }

// ctxKey is an unexported empty struct, so no other package can construct the
// key and inject a principal into a context without going through
// WithPrincipal. A string key would be forgeable by any import.
type ctxKey struct{}

// WithPrincipal returns a copy of ctx carrying p. Only RequireAuth should call
// this in production code; tests use it to build a pre-authenticated request.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the principal RequireAuth stored on the request
// context. The bool is false on any route that was not wrapped in RequireAuth,
// which handlers must treat as unauthenticated rather than as an empty user --
// a zero Principal has the zero UUID, and treating it as a real ID would match
// nothing at best and the wrong row at worst.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
