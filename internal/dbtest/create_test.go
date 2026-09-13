//go:build integration

package dbtest

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestOpen_IsSafeWhenSuitesStartTogether is the regression test for the CI
// flake.
//
// `go test ./...` starts one binary per package in parallel, so on a server
// where the databases do not exist yet, several of them call Open at the same
// instant. CREATE DATABASE checks the name and inserts into pg_database with
// nothing held between the two steps, so every caller passed the check and all
// but one failed with `duplicate key value violates unique constraint
// "pg_database_datname_index"` (23505). It is a first-run-only race, which is
// why it passed on every local run and on one CI run before failing on the next.
//
// Dropping the database first is what makes this exercise the creating path
// rather than the already-exists one -- without it the test proves nothing.
func TestOpen_IsSafeWhenSuitesStartTogether(t *testing.T) {
	ctx := context.Background()
	require.NotEmpty(t, os.Getenv("DATABASE_URL"), "DATABASE_URL is required for integration tests; run `make db-up` first")

	dropDatabase(t, Name(SuiteConcurrency))

	const callers = 8
	var (
		wg   sync.WaitGroup
		errs = make(chan error, callers)
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Open(ctx, SuiteConcurrency)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent Open lost the race to create the database")
	}
}

// dropDatabase removes one of this package's databases, so the next Open has to
// create it. Nothing holds a connection to a suite database between runs.
func dropDatabase(t *testing.T, name string) {
	t.Helper()
	require.True(t, strings.HasPrefix(name, prefix), "refusing to drop %q", name)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx, "drop database if exists "+name)
	require.NoError(t, err)
}
