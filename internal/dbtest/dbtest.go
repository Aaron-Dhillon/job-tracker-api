// Package dbtest provisions the throwaway databases the integration suites run
// against.
//
// It exists because those suites open by dropping the public schema, and doing
// that to the database DATABASE_URL names destroys whatever the developer who
// ran the tests was working on. So no suite ever touches that database.
//
// Each suite gets a database of its own rather than sharing one. `go test ./...`
// runs package test binaries in parallel, and on a shared database that means
// one suite's `drop schema public cascade` lands in the middle of another
// suite's run -- a failure that reads as "relation does not exist" a long way
// from its cause.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5"
)

// Suite names, one database each. Two suites sharing a name would share a
// database, which is the thing this package exists to prevent.
const (
	SuiteMigrations  = "migrations"  // internal/db
	SuiteAPI         = "api"         // test/integration
	SuiteConcurrency = "concurrency" // this package's own regression test
)

// prefix marks a database as one this package created. Nothing outside it is
// ever dropped.
const prefix = "jobtracker_test_"

// createLock is the advisory-lock key creation serializes on. The value is
// arbitrary -- it only has to be one this package agrees with itself on -- and
// spells "dbtest".
const createLock int64 = 0x646274657374

// Name returns the database belonging to a suite.
func Name(suite string) string { return prefix + suite }

// A DB is a database one suite owns outright.
type DB struct {
	// URL connects to it. Everything but the database name is copied from
	// DATABASE_URL, so a developer pointing at a non-default host, port or
	// password gets their test databases on that same server rather than a
	// connection refused.
	URL string

	// suite is set only by Open, which is what makes Reset's check meaningful:
	// there is no way to hand Reset a DB naming the developer's own database
	// and have it agree to drop the schema.
	suite string
}

// Open returns the suite's database, creating it if this is the first run
// against this server.
func Open(ctx context.Context, suite string) (*DB, error) {
	// Fail rather than skip. The `integration` build tag is an explicit opt-in,
	// so a missing DATABASE_URL means a misconfigured run, not an irrelevant
	// one -- and skipping would let CI report green having tested nothing.
	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		return nil, errors.New("DATABASE_URL is required for integration tests; run `make db-up` first")
	}

	name := Name(suite)
	dbURL, err := derive(adminURL, name)
	if err != nil {
		return nil, err
	}
	if err := create(ctx, adminURL, name); err != nil {
		return nil, fmt.Errorf("create database %s: %w", name, err)
	}
	return &DB{URL: dbURL, suite: suite}, nil
}

// Reset returns the database to an empty public schema.
//
// Dropping the schema also drops schema_migrations, so the caller's db.Up runs
// every migration from nothing -- which is what makes an edited migration take
// effect instead of being skipped as already applied.
//
// It opens its own connection and asks Postgres which database it landed in,
// rather than trusting the URL it was handed: the whole point of the check is
// to catch a URL that is not what it claims to be.
func (d *DB) Reset(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, d.URL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var name string
	if err := conn.QueryRow(ctx, "select current_database()").Scan(&name); err != nil {
		return err
	}
	if name != Name(d.suite) {
		return fmt.Errorf("refusing to drop the public schema of %q: the %s suite owns %q and nothing else", name, d.suite, Name(d.suite))
	}

	_, err = conn.Exec(ctx, "drop schema public cascade; create schema public;")
	return err
}

// derive swaps the database name in a libpq URL, leaving every other component
// -- host, port, credentials, sslmode -- exactly as it was.
func derive(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("DATABASE_URL has scheme %q, want postgres", u.Scheme)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// create makes the database if it is not there yet, under an advisory lock.
//
// The lock is the whole point. CREATE DATABASE checks the name and inserts into
// pg_database without holding anything between the two, so two backends racing
// on the same name both pass the check and the loser fails with a unique
// violation on pg_database_datname_index (23505) -- not the duplicate_database
// (42P04) that "it already exists" looks like, and so not something a caller
// can reasonably special-case. That race is what `go test ./...` sets up on a
// server where the databases do not exist yet, and it is what broke CI.
//
// Holding the lock across the check and the create removes it outright: at most
// one backend is ever between those two steps. Distinct names do not contend in
// practice, but serializing every creation costs a few hundred milliseconds once
// per server and needs no argument about which pairs are safe.
func create(ctx context.Context, adminURL, name string) error {
	// Connected to the database DATABASE_URL already names, not to a
	// maintenance database like `postgres`: that one is known to exist, because
	// the caller is configured to use it.
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	// Closing the connection releases the advisory lock, including on the
	// error paths below.
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", createLock); err != nil {
		return err
	}

	var exists bool
	if err := conn.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

	// CREATE DATABASE accepts no bind parameters and cannot run inside a
	// transaction. Interpolating is safe only because the name is built from
	// constants in this file, never from anything a caller supplies.
	_, err = conn.Exec(ctx, "create database "+name)
	return err
}
