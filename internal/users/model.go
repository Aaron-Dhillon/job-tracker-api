// Package users owns the user model, its repository, and the register/login
// handlers.
package users

import (
	"time"

	"github.com/google/uuid"
)

// User is a row of the users table.
//
// PasswordHash carries json:"-" so it cannot be serialised by accident. That
// tag is the whole defence: every handler here returns a User or a []User
// directly, so a tag removed in a refactor would leak hashes from
// GET /admin/users without any other code changing. users_test.go pins it.
type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

// Credentials is the body of both POST /auth/register and POST /auth/login.
type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// TokenResponse is the body of a successful login.
type TokenResponse struct {
	Token string `json:"token"`
}
