package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// DefaultTTL is the token lifetime from PRD §Auth. There are no refresh
// tokens (explicitly out of scope), so 24h is the whole session.
const DefaultTTL = 24 * time.Hour

// MinSecretLen is 32 bytes because HS256's security is capped by the key, not
// the algorithm: RFC 7518 §3.2 requires a key at least as long as the hash
// output. A short, guessable JWT_SECRET means anyone can mint an admin token,
// and no amount of correct verification code helps.
const MinSecretLen = 32

// ErrInvalidToken is returned for every rejected token: missing, malformed,
// wrong signature, wrong algorithm, expired, or carrying claims that make no
// sense. The reason is deliberately not exposed -- telling a caller "expired"
// versus "bad signature" turns the endpoint into an oracle for probing tokens.
var ErrInvalidToken = errors.New("invalid token")

// Claims is the token payload: the registered claims (sub, exp, iat) plus the
// role, which is what RequireRole reads.
//
// The role is carried in the token rather than looked up per request, which is
// the trade this design makes: no database round trip on every call, but a
// role change does not take effect until the current token expires. With a 24h
// TTL and no admin-demotion flow in scope, that is acceptable; it would not be
// if roles could be revoked for cause.
type Claims struct {
	jwt.RegisteredClaims
	Role string `json:"role"`
}

// Issuer mints and verifies tokens for one secret. Both directions live on the
// same type so they cannot drift apart -- an issuer that signs with one
// algorithm and a verifier that accepts another is the bug this guards.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

// NewIssuer validates the secret and returns an Issuer. A ttl of zero means
// DefaultTTL.
func NewIssuer(secret string, ttl time.Duration) (*Issuer, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("JWT_SECRET must be at least %d bytes, got %d", MinSecretLen, len(secret))
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Issuer{secret: []byte(secret), ttl: ttl}, nil
}

// Issue returns a signed HS256 token for the given user and role.
func (i *Issuer) Issue(userID uuid.UUID, role string) (string, error) {
	if !ValidRole(role) {
		return "", fmt.Errorf("issue token: unknown role %q", role)
	}

	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
		Role: role,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a token, returning the principal it describes.
// Every failure is ErrInvalidToken.
//
// The important line is jwt.WithValidMethods. Without it the parser trusts the
// "alg" header the *client* chose. The dangerous case is RS256: the library
// would hand our HMAC secret to the RSA verifier as if it were a public key,
// and a public key is by definition not secret -- anyone who learns it can
// forge tokens. Pinning the accepted algorithm to the one we sign with is what
// closes that.
//
// alg "none" is blocked twice over, and it is worth knowing which defence does
// the work: golang-jwt v5 refuses the none method unless the keyfunc returns
// its jwt.UnsafeAllowNoneSignatureType sentinel, which ours never does. So a
// none-token is rejected here even with WithValidMethods removed -- as the
// mutation test confirmed. WithValidMethods is the defence we own; the library
// guard is the one we inherit, and inherited guards are the ones that vanish
// in a major version bump.
func (i *Issuer) Verify(token string) (Principal, error) {
	var claims Claims

	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	// A signature only proves we minted the token, not that its contents still
	// make sense. Both claims are re-checked before anything downstream trusts
	// them as an identity.
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: subject is not a uuid", ErrInvalidToken)
	}
	if !ValidRole(claims.Role) {
		return Principal{}, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, claims.Role)
	}

	return Principal{ID: id, Role: claims.Role}, nil
}
