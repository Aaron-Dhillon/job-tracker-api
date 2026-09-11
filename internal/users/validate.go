package users

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
)

// MaxEmailLen is the RFC 5321 §4.5.3.1.3 limit on a forward path. The column
// is unbounded text, so without this a 1 MB body becomes a 1 MB email.
const MaxEmailLen = 254

// ErrInvalidCredentials covers every rejection of a registration body. The
// message names what is wrong, because at registration there is no account to
// leak -- unlike login, which must stay uniform.
var ErrInvalidCredentials = errors.New("invalid credentials")

// NormalizeEmail lowercases and trims an address.
//
// RFC 5321 makes the local part case-sensitive in theory; no mail provider
// treats it that way in practice. Storing addresses as typed would let
// Aaron@example.com and aaron@example.com register as two accounts, and the
// second login attempt would be a mystery. Case folding here means the unique
// index does the work.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateCredentials checks an email and password before either is hashed or
// written, and returns the normalized email.
func ValidateCredentials(email, password string) (string, error) {
	email = strings.TrimSpace(email)

	if email == "" {
		return "", fmt.Errorf("%w: email is required", ErrInvalidCredentials)
	}
	if len(email) > MaxEmailLen {
		return "", fmt.Errorf("%w: email must not exceed %d characters", ErrInvalidCredentials, MaxEmailLen)
	}

	// ParseAddress also accepts a display-name form, "Aaron <a@b.com>". That is
	// a valid address but not a valid account name, so anything the parser had
	// to reshape is rejected rather than silently stored differently.
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("%w: email is not a valid address", ErrInvalidCredentials)
	}

	switch {
	case len(password) < auth.MinPasswordLen:
		return "", fmt.Errorf("%w: password must be at least %d characters", ErrInvalidCredentials, auth.MinPasswordLen)
	case len(password) > auth.MaxPasswordLen:
		// bcrypt reads at most 72 bytes and errors past that rather than
		// truncating, so this is the algorithm's limit, not a policy.
		return "", fmt.Errorf("%w: password must not exceed %d bytes", ErrInvalidCredentials, auth.MaxPasswordLen)
	}

	return NormalizeEmail(email), nil
}
