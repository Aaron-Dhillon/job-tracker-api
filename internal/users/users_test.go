package users_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/users"
)

// The json:"-" tag is the only thing standing between GET /admin/users and a
// response full of bcrypt hashes. Every handler in this package returns User
// values directly, so if the tag goes, the hashes ship.
func TestUser_NeverSerialisesPasswordHash(t *testing.T) {
	user := users.User{
		ID:           uuid.New(),
		Email:        "aaron@example.com",
		PasswordHash: "$2a$12$SOME.REAL.LOOKING.HASH.VALUE.THAT.MUST.NOT.LEAK",
		Role:         auth.RoleUser,
		CreatedAt:    time.Now(),
	}

	encoded, err := json.Marshal(user)
	require.NoError(t, err)

	assert.NotContains(t, string(encoded), "$2a$12$")
	assert.NotContains(t, string(encoded), "password")
	assert.NotContains(t, string(encoded), "hash")

	var round map[string]any
	require.NoError(t, json.Unmarshal(encoded, &round))
	assert.ElementsMatch(t, []string{"id", "email", "role", "created_at"}, keysOf(round))
}

func TestUser_ListSerialisesWithoutHashes(t *testing.T) {
	list := []users.User{
		{ID: uuid.New(), Email: "a@example.com", PasswordHash: "$2a$12$aaa", Role: auth.RoleAdmin},
		{ID: uuid.New(), Email: "b@example.com", PasswordHash: "$2a$12$bbb", Role: auth.RoleUser},
	}

	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "$2a$12$")
}

func TestNormalizeEmail(t *testing.T) {
	// Case folding means the unique index does the work. Without it,
	// Aaron@example.com and aaron@example.com are two accounts and the second
	// login attempt is an unexplainable failure.
	assert.Equal(t, "aaron@example.com", users.NormalizeEmail("  Aaron@Example.COM  "))
	assert.Equal(t, "", users.NormalizeEmail("   "))
}

func TestValidateCredentials(t *testing.T) {
	const goodPassword = "correct horse battery"

	tests := []struct {
		name      string
		email     string
		password  string
		wantEmail string
		wantErr   string
	}{
		{name: "valid", email: "aaron@example.com", password: goodPassword, wantEmail: "aaron@example.com"},
		{name: "normalizes", email: "  Aaron@Example.COM ", password: goodPassword, wantEmail: "aaron@example.com"},
		{name: "plus addressing", email: "aaron+jobs@example.com", password: goodPassword, wantEmail: "aaron+jobs@example.com"},
		{name: "empty email", email: "", password: goodPassword, wantErr: "email is required"},
		{name: "no at sign", email: "not-an-email", password: goodPassword, wantErr: "not a valid address"},
		{name: "no domain", email: "aaron@", password: goodPassword, wantErr: "not a valid address"},
		{
			// mail.ParseAddress accepts this, but it is an address with a
			// display name, not an account name.
			name: "display-name form", email: "Aaron <aaron@example.com>", password: goodPassword,
			wantErr: "not a valid address",
		},
		{
			name: "over-long email", email: strings.Repeat("a", 250) + "@example.com", password: goodPassword,
			wantErr: "must not exceed 254",
		},
		{name: "short password", email: "aaron@example.com", password: "short", wantErr: "at least 8"},
		{
			name: "password past bcrypt's limit", email: "aaron@example.com",
			password: strings.Repeat("a", auth.MaxPasswordLen+1), wantErr: "must not exceed 72",
		},
		{
			name: "password exactly at bcrypt's limit", email: "aaron@example.com",
			password: strings.Repeat("a", auth.MaxPasswordLen), wantEmail: "aaron@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email, err := users.ValidateCredentials(tt.email, tt.password)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorIs(t, err, users.ErrInvalidCredentials)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantEmail, email)
		})
	}
}

// The length limit is in bytes because that is what bcrypt reads. A password
// of 72 multi-byte characters is well past the limit even though it "looks"
// shorter.
func TestValidateCredentials_PasswordLimitIsBytes(t *testing.T) {
	_, err := users.ValidateCredentials("aaron@example.com", strings.Repeat("é", 40))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not exceed 72 bytes")
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
