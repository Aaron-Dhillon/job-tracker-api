//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/db"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/dbtest"
)

// freshDB drops and recreates the public schema, then migrates up. Dropping the
// schema also clears schema_migrations, so every test starts from nothing --
// including after a migration file changes, which is the case that a skipped
// "already applied" version would quietly break.
//
// The database is dbtest's, never the one DATABASE_URL names: a helper whose
// first act is `drop schema public cascade` must not be pointed at whatever
// the developer running the tests was working on.
func freshDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()

	url, err := dbtest.URL(ctx)
	require.NoError(t, err)
	require.NoError(t, dbtest.Reset(ctx, url))
	require.NoError(t, db.Up(url))

	pool, err := db.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return url, pool
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestMigrateUp_CreatesSchema(t *testing.T) {
	_, pool := freshDB(t)
	ctx := context.Background()

	for _, table := range []string{"users", "companies", "applications", "status_transitions"} {
		var reg *string
		require.NoError(t, pool.QueryRow(ctx, "select to_regclass($1)::text", "public."+table).Scan(&reg))
		require.NotNil(t, reg, "table %s should exist", table)
	}

	var indexdef string
	err := pool.QueryRow(ctx,
		"select indexdef from pg_indexes where schemaname = 'public' and indexname = 'applications_search_idx'",
	).Scan(&indexdef)
	require.NoError(t, err, "applications_search_idx should exist")
	assert.Contains(t, indexdef, "USING gin", "search index must be GIN")

	for _, idx := range []string{"applications_user_idx", "applications_company_idx", "status_transitions_app_idx"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			"select count(*) from pg_indexes where schemaname = 'public' and indexname = $1", idx).Scan(&n))
		assert.Equal(t, 1, n, "index %s should exist", idx)
	}
}

func TestMigrateUp_GeneratedColumn(t *testing.T) {
	_, pool := freshDB(t)
	ctx := context.Background()

	var isGenerated string
	require.NoError(t, pool.QueryRow(ctx, `
		select is_generated from information_schema.columns
		where table_name = 'applications' and column_name = 'search_vec'`).Scan(&isGenerated))
	assert.Equal(t, "ALWAYS", isGenerated)

	appID := seedApplication(t, pool, "owner@example.com", "Visa",
		"Senior Backend Engineer", "Austin, TX", "golang and postgres shop")

	var found string
	err := pool.QueryRow(ctx, `
		select id::text from applications
		where search_vec @@ plainto_tsquery('english', 'engineer')`).Scan(&found)
	require.NoError(t, err, "generated tsvector should match on role_title")
	assert.Equal(t, appID, found)

	// English stemming: "shop" in the notes matches the query "shops".
	err = pool.QueryRow(ctx, `
		select id::text from applications
		where search_vec @@ plainto_tsquery('english', 'shops')`).Scan(&found)
	require.NoError(t, err, "tsvector should be stemmed with the english config")
	assert.Equal(t, appID, found)
}

func TestMigrateUp_StatusCheck(t *testing.T) {
	_, pool := freshDB(t)
	ctx := context.Background()

	appID := seedApplication(t, pool, "owner@example.com", "Visa", "Backend Engineer", "Austin, TX", "")

	_, err := pool.Exec(ctx, "update applications set status = 'bogus' where id = $1", appID)
	require.Error(t, err, "status check constraint should reject unknown states")
	assert.Equal(t, "23514", sqlState(err), "expected a check_violation")

	for _, ok := range []string{"applied", "screening", "interviewing", "offer", "rejected", "withdrawn"} {
		_, err := pool.Exec(ctx, "update applications set status = $1 where id = $2", ok, appID)
		assert.NoError(t, err, "status %q should be accepted", ok)
	}
}

// TestMigrateUp_ChangedByCascade covers the ON DELETE CASCADE added to
// status_transitions.changed_by. The author here is deliberately not the
// application owner, so only the changed_by FK can clean the row up.
func TestMigrateUp_ChangedByCascade(t *testing.T) {
	_, pool := freshDB(t)
	ctx := context.Background()

	appID := seedApplication(t, pool, "owner@example.com", "Visa", "Backend Engineer", "Austin, TX", "")

	var authorID string
	require.NoError(t, pool.QueryRow(ctx, `
		insert into users (email, password_hash, role) values ('admin@example.com', 'x', 'admin')
		returning id::text`).Scan(&authorID))

	_, err := pool.Exec(ctx, `
		insert into status_transitions (application_id, from_status, to_status, changed_by)
		values ($1, 'applied', 'screening', $2)`, appID, authorID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "delete from users where id = $1", authorID)
	require.NoError(t, err, "deleting a transition author must not raise an FK violation")

	var transitions, apps int
	require.NoError(t, pool.QueryRow(ctx, "select count(*) from status_transitions").Scan(&transitions))
	require.NoError(t, pool.QueryRow(ctx, "select count(*) from applications").Scan(&apps))
	assert.Equal(t, 0, transitions, "the transition row should cascade away")
	assert.Equal(t, 1, apps, "the application belongs to another user and must survive")
}

func TestMigrate_UpDownUp(t *testing.T) {
	url, pool := freshDB(t)
	ctx := context.Background()

	require.NoError(t, db.Down(url))

	var reg *string
	require.NoError(t, pool.QueryRow(ctx, "select to_regclass('public.applications')::text").Scan(&reg))
	assert.Nil(t, reg, "down migration should drop applications")

	require.NoError(t, db.Up(url), "re-applying migrations should succeed")
	require.NoError(t, pool.QueryRow(ctx, "select to_regclass('public.applications')::text").Scan(&reg))
	assert.NotNil(t, reg, "up migration should recreate applications")

	// Up on an already-current database is a no-op, not an error.
	assert.NoError(t, db.Up(url))
}

func TestNew_RejectsUnreachableDatabase(t *testing.T) {
	ctx := context.Background()

	_, err := db.New(ctx, "not-a-url")
	assert.Error(t, err)

	_, err = db.New(ctx, "postgres://postgres:postgres@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2")
	assert.Error(t, err, "an unreachable host must fail the ping")
}

// seedApplication inserts a user, a company and one application, returning the
// application id.
func seedApplication(t *testing.T, pool *pgxpool.Pool, email, company, title, location, notes string) string {
	t.Helper()
	ctx := context.Background()

	var userID, companyID, appID string
	require.NoError(t, pool.QueryRow(ctx, `
		insert into users (email, password_hash, role) values ($1, 'x', 'user')
		returning id::text`, email).Scan(&userID))
	require.NoError(t, pool.QueryRow(ctx, `
		insert into companies (name) values ($1) returning id::text`, company).Scan(&companyID))
	require.NoError(t, pool.QueryRow(ctx, `
		insert into applications (user_id, company_id, role_title, location, notes)
		values ($1, $2, $3, nullif($4, ''), nullif($5, ''))
		returning id::text`, userID, companyID, title, location, notes).Scan(&appID))

	return appID
}
