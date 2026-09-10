package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Cost is bcrypt's work factor, fixed at 12 by PRD §Auth. Each increment
// doubles the work; 12 is roughly 250ms on commodity hardware, which is slow
// enough to make offline cracking expensive and fast enough for a login.
const Cost = 12

// MinPasswordLen is the shortest password accepted at registration.
//
// MaxPasswordLen is bcrypt's own hard limit, not a policy choice: the
// algorithm reads at most 72 bytes of input. Older wrappers silently truncated
// past that -- so "correct horse battery staple ..." and a 200-byte variant of
// it would share a hash. x/crypto returns bcrypt.ErrPasswordTooLong instead,
// and the register handler maps that to 400 (phase 4).
const (
	MinPasswordLen = 8
	MaxPasswordLen = 72
)

// ErrPasswordTooLong is returned for input past bcrypt's 72-byte limit.
// It wraps the x/crypto sentinel, so errors.Is works against either.
var ErrPasswordTooLong = bcrypt.ErrPasswordTooLong

// dummyHash is a valid cost-12 bcrypt hash of a random string that is nobody's
// password. It exists so DummyCheck can burn the same CPU time as a real
// comparison; see DummyCheck for why that matters. It is not a credential and
// unlocks nothing.
const dummyHash = "$2a$12$NwmKlZak1uRl1FLUu7RSbucoyzTR47AUxLXP1UnoZ.bSs.EMo/D4W"

// HashPassword returns a bcrypt hash of plain at Cost.
//
// bcrypt salts every hash internally, so the same password hashed twice gives
// two different strings -- there is no separate salt column, and hashes must
// never be compared with ==.
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), Cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword reports whether plain matches hash. Any non-nil error means
// "do not authenticate" -- callers must not branch on which error it is, and
// must not report it to the client. Login answers 401 with one message
// whatever the reason.
func CheckPassword(hash, plain string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return fmt.Errorf("check password: %w", err)
	}
	return nil
}

// DummyCheck spends the same bcrypt time as CheckPassword against a throwaway
// hash, and discards the result.
//
// Login calls it when no user matched the email. Without it, an unknown email
// returns in microseconds and a known one takes ~250ms of bcrypt, so response
// latency answers "is this address registered?" -- which is exactly what the
// single identical 401 exists to hide. Returning the same body but not the
// same timing is a half-closed door.
func DummyCheck(plain string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(plain))
}

// HashCost reports the cost a hash was generated with, for tests and for
// spotting hashes written under an older policy.
func HashCost(hash string) (int, error) {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return 0, fmt.Errorf("read bcrypt cost: %w", err)
	}
	return cost, nil
}

// ErrMismatch reports whether err is a wrong-password result rather than a
// malformed hash. Nothing in the request path should need this -- it is here
// for tests and for the seed-admin flow, which distinguishes "hash needs
// refreshing" from "password is wrong".
func ErrMismatch(err error) bool {
	return errors.Is(err, bcrypt.ErrMismatchedHashAndPassword)
}
