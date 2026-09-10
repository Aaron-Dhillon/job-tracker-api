package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
)

// Long enough to satisfy MinSecretLen; the value itself is meaningless.
const (
	testSecret  = "test-secret-that-is-at-least-32-bytes-long"
	otherSecret = "a-completely-different-secret-of-adequate-length"
)

func newIssuer(t *testing.T) *auth.Issuer {
	t.Helper()
	issuer, err := auth.NewIssuer(testSecret, auth.DefaultTTL)
	require.NoError(t, err)
	return issuer
}

func TestNewIssuer_RejectsShortSecret(t *testing.T) {
	// HS256 is only as strong as its key. A short JWT_SECRET is brute-forceable
	// offline from a single captured token, and then anyone can mint an admin.
	_, err := auth.NewIssuer("short", auth.DefaultTTL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32 bytes")

	_, err = auth.NewIssuer("", auth.DefaultTTL)
	assert.Error(t, err)

	_, err = auth.NewIssuer(strings.Repeat("k", auth.MinSecretLen), auth.DefaultTTL)
	assert.NoError(t, err, "exactly MinSecretLen must be accepted")
}

// A zero ttl means "use the PRD default" rather than "expires immediately",
// which is the difference between a working login and every token being dead
// on arrival.
func TestNewIssuer_ZeroTTLFallsBackToDefault(t *testing.T) {
	issuer, err := auth.NewIssuer(testSecret, 0)
	require.NoError(t, err)

	before := time.Now()
	token, err := issuer.Issue(uuid.New(), auth.RoleUser)
	require.NoError(t, err)

	claims := decodeClaims(t, token)
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	assert.WithinDuration(t, before.Add(auth.DefaultTTL), exp, time.Minute)

	_, err = issuer.Verify(token)
	assert.NoError(t, err)
}

func TestIssue_Verify_RoundTrip(t *testing.T) {
	issuer := newIssuer(t)
	id := uuid.New()

	token, err := issuer.Issue(id, auth.RoleAdmin)
	require.NoError(t, err)

	principal, err := issuer.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, id, principal.ID)
	assert.Equal(t, auth.RoleAdmin, principal.Role)
	assert.True(t, principal.IsAdmin())
}

func TestIssue_StampsClaimsFromThePRD(t *testing.T) {
	issuer := newIssuer(t)
	id := uuid.New()
	before := time.Now()

	token, err := issuer.Issue(id, auth.RoleUser)
	require.NoError(t, err)

	claims := decodeClaims(t, token)
	assert.Equal(t, id.String(), claims["sub"], "sub carries the user id")
	assert.Equal(t, auth.RoleUser, claims["role"])
	require.Contains(t, claims, "iat")
	require.Contains(t, claims, "exp")

	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	assert.WithinDuration(t, before.Add(24*time.Hour), exp, time.Minute, "24h expiry per PRD")
}

func TestIssue_RejectsUnknownRole(t *testing.T) {
	issuer := newIssuer(t)
	_, err := issuer.Issue(uuid.New(), "superuser")
	assert.Error(t, err, "a role outside the schema check constraint must never be minted")
}

func TestVerify_RejectsBadTokens(t *testing.T) {
	issuer := newIssuer(t)
	valid, err := issuer.Issue(uuid.New(), auth.RoleUser)
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-token"},
		{"two segments", strings.Join(strings.Split(valid, ".")[:2], ".")},
		{"tampered payload", tamperRole(t, valid, auth.RoleAdmin)},
		{"tampered signature", valid[:len(valid)-3] + "AAA"},
		{"signed with another secret", signWith(t, otherSecret, jwt.SigningMethodHS256, defaultClaims(uuid.New(), auth.RoleUser))},
		{"expired", signWith(t, testSecret, jwt.SigningMethodHS256, expiredClaims(uuid.New(), auth.RoleUser))},
		{"no exp claim", signWith(t, testSecret, jwt.SigningMethodHS256, jwt.MapClaims{"sub": uuid.New().String(), "role": auth.RoleUser})},
		{"sub is not a uuid", signWith(t, testSecret, jwt.SigningMethodHS256, withSub(defaultClaims(uuid.New(), auth.RoleUser), "1 or 1=1"))},
		{"unknown role", signWith(t, testSecret, jwt.SigningMethodHS256, defaultClaims(uuid.New(), "superuser"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := issuer.Verify(tt.token)
			require.Error(t, err)
			assert.ErrorIs(t, err, auth.ErrInvalidToken)
		})
	}
}

// The classic forgery: a hand-written token with no signature at all, claiming
// admin. Two independent things reject it -- golang-jwt v5's refusal to run the
// none method without its unsafe sentinel, and our WithValidMethods pin -- so
// this test still passes if either one is removed. That is the point of
// keeping it: it pins the *outcome*, and TestVerify_RejectsOtherAlgorithms
// pins the algorithm allowlist that we control.
func TestVerify_RejectsAlgNone(t *testing.T) {
	issuer := newIssuer(t)
	admin := uuid.New()

	forged := algNoneToken(t, jwt.MapClaims{
		"sub":  admin.String(),
		"role": auth.RoleAdmin,
		"exp":  time.Now().Add(time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	})

	// Sanity: the forgery is well-formed enough that a permissive parser would
	// accept it, so the rejection below is the pinning we care about and not an
	// accident of malformed input.
	require.Len(t, strings.Split(forged, "."), 3)
	require.Equal(t, "", strings.Split(forged, ".")[2], "alg:none carries an empty signature")

	_, err := issuer.Verify(forged)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

// Algorithm confusion in the other direction: a token that is genuinely signed
// with our secret but declares a different HMAC variant. Only the algorithm we
// sign with is accepted.
func TestVerify_RejectsOtherAlgorithms(t *testing.T) {
	issuer := newIssuer(t)

	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
		t.Run(method.Alg(), func(t *testing.T) {
			token := signWith(t, testSecret, method, defaultClaims(uuid.New(), auth.RoleAdmin))
			_, err := issuer.Verify(token)
			require.Error(t, err)
			assert.ErrorIs(t, err, auth.ErrInvalidToken)
		})
	}
}

// Every rejection reads the same to a caller. Distinguishing "expired" from
// "bad signature" would let an attacker sort captured tokens for free.
func TestVerify_ErrorIsUniform(t *testing.T) {
	issuer := newIssuer(t)

	expired := signWith(t, testSecret, jwt.SigningMethodHS256, expiredClaims(uuid.New(), auth.RoleUser))
	wrongKey := signWith(t, otherSecret, jwt.SigningMethodHS256, defaultClaims(uuid.New(), auth.RoleUser))

	_, expiredErr := issuer.Verify(expired)
	_, wrongKeyErr := issuer.Verify(wrongKey)

	require.ErrorIs(t, expiredErr, auth.ErrInvalidToken)
	require.ErrorIs(t, wrongKeyErr, auth.ErrInvalidToken)
	// The wrapped detail differs for logs; what reaches the client is the
	// sentinel's own text, which is identical either way.
	assert.Equal(t, "invalid token", auth.ErrInvalidToken.Error())
}

// Two issuers with different secrets must not accept each other's tokens --
// rotating JWT_SECRET has to actually invalidate the old ones.
func TestVerify_SecretIsolation(t *testing.T) {
	mine := newIssuer(t)
	theirs, err := auth.NewIssuer(otherSecret, auth.DefaultTTL)
	require.NoError(t, err)

	token, err := theirs.Issue(uuid.New(), auth.RoleUser)
	require.NoError(t, err)

	_, err = mine.Verify(token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

// --- helpers ---------------------------------------------------------------

func defaultClaims(id uuid.UUID, role string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"sub":  id.String(),
		"role": role,
		"iat":  now.Unix(),
		"exp":  now.Add(time.Hour).Unix(),
	}
}

func expiredClaims(id uuid.UUID, role string) jwt.MapClaims {
	past := time.Now().Add(-48 * time.Hour)
	return jwt.MapClaims{
		"sub":  id.String(),
		"role": role,
		"iat":  past.Unix(),
		"exp":  past.Add(time.Hour).Unix(),
	}
}

func withSub(claims jwt.MapClaims, sub string) jwt.MapClaims {
	claims["sub"] = sub
	return claims
}

func signWith(t *testing.T, secret string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return token
}

// algNoneToken hand-builds the classic forgery: a JWT whose header claims no
// algorithm and whose signature segment is empty.
func algNoneToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := map[string]string{"alg": "none", "typ": "JWT"}
	return enc(header) + "." + enc(claims) + "."
}

// tamperRole rewrites the role in a signed token's payload without resigning,
// which is what an attacker with a user token and no secret can actually do.
func tamperRole(t *testing.T, token, role string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	claims["role"] = role

	rewritten, err := json.Marshal(claims)
	require.NoError(t, err)
	parts[1] = base64.RawURLEncoding.EncodeToString(rewritten)
	return strings.Join(parts, ".")
}

func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}
