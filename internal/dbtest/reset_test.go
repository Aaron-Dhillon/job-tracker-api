//go:build integration

package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReset_RefusesADatabaseItDidNotName is the test that makes the guard worth
// having. Reset's body is `drop schema public cascade`, so the interesting case
// is not that it works -- every integration run proves that -- but that it
// declines when the URL turns out to name the database a developer is using.
//
// The DB below is hand-built precisely because Open cannot produce it: Open
// always derives the name, so reaching this state at all means a caller went
// around it.
func TestReset_RefusesADatabaseItDidNotName(t *testing.T) {
	ctx := context.Background()

	devURL := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, devURL, "DATABASE_URL is required for integration tests; run `make db-up` first")
	require.NotEqual(t, Name(SuiteMigrations), currentDatabase(t, devURL),
		"DATABASE_URL already names a suite database; this test cannot prove anything")

	d := &DB{URL: devURL, suite: SuiteMigrations}

	err := d.Reset(ctx)
	require.Error(t, err, "Reset dropped the schema of a database it did not name")
	assert.ErrorContains(t, err, "refusing")
}

func currentDatabase(t *testing.T, url string) string {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var name string
	require.NoError(t, conn.QueryRow(ctx, "select current_database()").Scan(&name))
	return name
}
