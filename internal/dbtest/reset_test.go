//go:build integration

package dbtest_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/dbtest"
)

// TestReset_RefusesAnyOtherDatabase is the test that makes the guard worth
// having. Reset's body is `drop schema public cascade`, so the interesting
// case is not that it works -- every integration run proves that -- but that
// it declines when handed the URL of the database a developer is using.
func TestReset_RefusesAnyOtherDatabase(t *testing.T) {
	ctx := context.Background()

	devURL := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, devURL, "DATABASE_URL is required for integration tests; run `make db-up` first")

	testURL, err := dbtest.URL(ctx)
	require.NoError(t, err)
	require.NotEqual(t, devURL, testURL, "DATABASE_URL already names the test database; this test cannot prove anything")

	err = dbtest.Reset(ctx, devURL)
	require.Error(t, err, "Reset wiped a database that is not %s", dbtest.Name)
	assert.ErrorContains(t, err, "refusing")
}
