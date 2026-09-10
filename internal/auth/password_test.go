package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	const password = "correct horse battery"

	hash, err := auth.HashPassword(password)
	require.NoError(t, err)

	assert.NoError(t, auth.CheckPassword(hash, password))
	assert.Error(t, auth.CheckPassword(hash, password+"!"), "a wrong password must not verify")
	assert.Error(t, auth.CheckPassword(hash, ""), "an empty password must not verify")
}

// The cost is a security parameter, not a default -- if a refactor drops the
// argument, bcrypt silently falls back to 10, which is four times cheaper to
// crack. This test is what notices.
func TestHashPassword_UsesCost12(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery")
	require.NoError(t, err)

	cost, err := auth.HashCost(hash)
	require.NoError(t, err)
	assert.Equal(t, 12, cost)
	assert.Equal(t, 12, auth.Cost)
}

// Two hashes of the same password must differ: bcrypt salts internally, and a
// hash column that repeats would let anyone spot shared passwords across rows.
func TestHashPassword_SaltsEachHash(t *testing.T) {
	first, err := auth.HashPassword("correct horse battery")
	require.NoError(t, err)
	second, err := auth.HashPassword("correct horse battery")
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.NoError(t, auth.CheckPassword(first, "correct horse battery"))
	assert.NoError(t, auth.CheckPassword(second, "correct horse battery"))
}

// Past 72 bytes bcrypt errors rather than truncating. If it truncated, every
// password sharing its first 72 bytes would share a hash; the register handler
// turns this error into a 400 instead.
func TestHashPassword_RejectsOver72Bytes(t *testing.T) {
	atLimit := strings.Repeat("a", auth.MaxPasswordLen)
	_, err := auth.HashPassword(atLimit)
	assert.NoError(t, err, "72 bytes is the limit, not one past it")

	_, err = auth.HashPassword(atLimit + "a")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrPasswordTooLong)
}

func TestHashCost_RejectsNonHash(t *testing.T) {
	_, err := auth.HashCost("not-a-bcrypt-hash")
	assert.Error(t, err, "a cost read must fail loudly rather than report 0")
}

func TestCheckPassword_RejectsMalformedHash(t *testing.T) {
	err := auth.CheckPassword("not-a-bcrypt-hash", "correct horse battery")
	require.Error(t, err)
	assert.False(t, auth.ErrMismatch(err), "a corrupt hash is not the same as a wrong password")

	hash, err := auth.HashPassword("correct horse battery")
	require.NoError(t, err)
	assert.True(t, auth.ErrMismatch(auth.CheckPassword(hash, "wrong")))
}

// DummyCheck exists to make an unknown email cost the same as a known one. If
// its hash were malformed, bcrypt would return in microseconds and the timing
// side channel it was written to close would be wide open again.
func TestDummyCheck_RunsRealBcryptWork(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery")
	require.NoError(t, err)

	real := timeIt(func() { _ = auth.CheckPassword(hash, "some attempt") })
	dummy := timeIt(func() { auth.DummyCheck("some attempt") })

	// Same cost factor, so the same order of magnitude. A malformed dummy hash
	// returns in ~0, which fails this comfortably.
	assert.Greater(t, dummy*4, real, "DummyCheck must not be dramatically faster than a real check")
	assert.NotPanics(t, func() { auth.DummyCheck("") })
}
