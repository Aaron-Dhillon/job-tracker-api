package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDerive pins the one thing derive must never get wrong: it changes the
// database name and nothing else. Dropping the password, the port or sslmode
// on the way through would send the suites at some other server -- or at the
// same one under a different identity -- and the failure would read as a
// connection problem rather than a bug here.
func TestDerive(t *testing.T) {
	t.Run("swaps only the database name", func(t *testing.T) {
		got, err := derive("postgres://alice:s3cret@db.internal:5433/jobtracker?sslmode=require&application_name=api")
		require.NoError(t, err)
		assert.Equal(t, "postgres://alice:s3cret@db.internal:5433/"+Name+"?sslmode=require&application_name=api", got)
	})

	t.Run("accepts the postgresql scheme", func(t *testing.T) {
		got, err := derive("postgresql://postgres:postgres@localhost:5432/jobtracker?sslmode=disable")
		require.NoError(t, err)
		assert.Equal(t, "postgresql://postgres:postgres@localhost:5432/"+Name+"?sslmode=disable", got)
	})

	t.Run("is idempotent", func(t *testing.T) {
		once, err := derive("postgres://postgres@localhost:5432/jobtracker")
		require.NoError(t, err)
		twice, err := derive(once)
		require.NoError(t, err)
		assert.Equal(t, once, twice)
	})

	t.Run("rejects a non-postgres scheme", func(t *testing.T) {
		// A mysql:// or an http:// URL here means DATABASE_URL was pasted from
		// the wrong place, and pgx would report it much further downstream.
		_, err := derive("mysql://root@localhost:3306/jobtracker")
		assert.ErrorContains(t, err, "scheme")
	})

	t.Run("rejects a value that is not a URL", func(t *testing.T) {
		_, err := derive("host=localhost user=postgres dbname=jobtracker")
		assert.Error(t, err)
	})
}
