package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
)

// EnsureAdmin creates or refreshes the seed admin account.
//
// It is idempotent and safe on every start: the role is forced to admin and
// the hash is rewritten, so rotating ADMIN_PASSWORD actually takes effect
// rather than silently doing nothing because the row already exists. One
// implementation serves both callers -- startup wiring and `make seed-admin`
// -- because two copies of this would drift, and the one that drifts is the
// one that stops forcing the role.
func EnsureAdmin(ctx context.Context, repo *Repo, email, password string) (User, error) {
	normalized, err := ValidateCredentials(email, password)
	if err != nil {
		return User{}, fmt.Errorf("seed admin: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("seed admin: %w", err)
	}

	user, err := repo.Upsert(ctx, normalized, hash, auth.RoleAdmin)
	if err != nil {
		return User{}, fmt.Errorf("seed admin: %w", err)
	}
	return user, nil
}

// ErrAdminNotConfigured means ADMIN_EMAIL and ADMIN_PASSWORD were not both
// set. Startup treats this as "nothing to do"; `make seed-admin` treats it as
// a failure, since the whole point of that command is to create the account.
var ErrAdminNotConfigured = errors.New("ADMIN_EMAIL and ADMIN_PASSWORD must both be set")
