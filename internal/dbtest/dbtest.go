// Package dbtest provisions the throwaway database the integration suites run
// against.
//
// It exists because those suites drop and recreate the public schema, and doing
// that to the database DATABASE_URL names destroys whatever the developer who
// ran the tests was working on. So neither suite touches that database: they
// run against a sibling created here, on the same server, and Reset refuses to
// wipe anything else.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Name is the database the suites own outright. The suffix is load bearing:
// Reset checks for this exact name before it drops anything.
const Name = "jobtracker_test"

// duplicateDatabase is SQLSTATE 42P04, raised by CREATE DATABASE when the name
// is taken -- which, after the first run, it always is.
const duplicateDatabase = "42P04"

// URL returns a connection string for the test database, creating the database
// first if it does not exist.
//
// Everything but the database name comes from DATABASE_URL, so a developer
// pointing at a non-default host, port or password gets the test database on
// that server rather than a connection refused.
func URL(ctx context.Context) (string, error) {
	// Fail rather than skip. The `integration` build tag is an explicit opt-in,
	// so a missing DATABASE_URL means a misconfigured run, not an irrelevant
	// one -- and skipping would let CI report green having tested nothing.
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		return "", errors.New("DATABASE_URL is required for integration tests; run `make db-up` first")
	}

	testURL, err := derive(raw)
	if err != nil {
		return "", err
	}
	if err := create(ctx, raw); err != nil {
		return "", fmt.Errorf("create database %s: %w", Name, err)
	}
	return testURL, nil
}

// Reset returns the test database to an empty public schema.
//
// Dropping the schema also drops schema_migrations, so the caller's db.Up runs
// every migration from nothing -- which is what makes a changed migration take
// effect instead of being skipped as already applied.
//
// It opens its own connection so the name check cannot be bypassed by handing
// it a pool that was built from some other URL.
func Reset(ctx context.Context, testURL string) error {
	conn, err := pgx.Connect(ctx, testURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var name string
	if err := conn.QueryRow(ctx, "select current_database()").Scan(&name); err != nil {
		return err
	}
	if name != Name {
		return fmt.Errorf("refusing to drop the public schema of %q; the integration suites only ever reset %q", name, Name)
	}

	_, err = conn.Exec(ctx, "drop schema public cascade; create schema public;")
	return err
}

// derive swaps the database name in a libpq URL, leaving every other component
// -- host, port, credentials, sslmode -- exactly as it was.
func derive(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("DATABASE_URL has scheme %q, want postgres", u.Scheme)
	}
	u.Path = "/" + Name
	return u.String(), nil
}

// create runs CREATE DATABASE from a connection to the database DATABASE_URL
// already names, rather than to a maintenance database like `postgres`: that
// one is known to exist, because the caller is configured to use it.
func create(ctx context.Context, adminURL string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	// CREATE DATABASE accepts no bind parameters and cannot run inside a
	// transaction. Interpolating the name is safe only because it is a
	// compile-time constant in this file, never anything a caller supplies.
	_, err = conn.Exec(ctx, "create database "+Name)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == duplicateDatabase {
		return nil
	}
	return err
}
