# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

The product requirements are in docs/PRD.md. Read it before any change.
The stack, repo layout, schema, and endpoints are fixed. Do not substitute
libraries or add anything listed under "Explicitly out of scope."
Go version is set in go.mod; CI reads it via go-version-file. Project complete
as of Sept 16, 2026; the MVP scope is closed — see "Scope discipline". Run the
relevant tests after every change and show the output.

## Current state

All six phases are built, verified end to end, and merged to `main` (PR #1 the CI/CD phase, PR #2 the integration-database flake fix).

The Render deploy at https://job-tracker-api-8zqe.onrender.com, against Supabase, runs the **full API** — not the Phase-1 skeleton it started as. Confirmed 2026-09-16 by read-only probe: `/healthz` 200, protected routes 401 with `WWW-Authenticate: Bearer`, unknown routes JSON 404. The production admin was seeded from a laptop — `go run ./cmd/api -seed-admin` with `DATABASE_URL` pointed at Supabase — and that is how to re-seed it or rotate its password. Render carries no `ADMIN_*` env vars, so the boot-time `ensureAdmin` is a no-op there; the container never touches that account.

CI/CD is proven, not just written: CI green on both PRs and on `main`, and `Deploy` fired through `workflow_run` on the second merge and succeeded. As predicted, the merge that *landed* `deploy.yml` on `main` did not trigger it — that rule has now been paid for once; don't debug it again. `RENDER_DEPLOY_HOOK_URL` is set as a repo secret.

One thing that stays true of the live environment: `JWT_SECRET` on Render must be at least 32 bytes or the container refuses to start (`auth.MinSecretLen`). `EnsureAdmin` is an upsert that forces the role and rewrites the hash, so re-running the seed command against production rotates the password rather than creating a second account.

`docs/PRD.md` is the authoritative spec. `docs/PLAN.md` is the six-phase build order derived from it — it records which files each phase adds, the tests that prove it, and every place the PRD was ambiguous along with the decision taken. Read both before writing anything.

## Scope discipline

Project complete as of Sept 16, 2026. The MVP scope is closed; further changes are maintenance only. The out-of-scope list still applies — the PRD states it explicitly (refresh tokens, password reset, rate limiting, OpenAPI docs, frontend, pagination cursors, soft deletes, metrics, sub-resources). Do not build anything not named in the PRD, and do not substitute libraries — the stack is pinned deliberately.

## Commands

- `make db-up` / `make db-down` / `make db-reset` — postgres container (db-reset drops the volume)
- `make migrate` — apply migrations (`go run ./cmd/api -migrate-only`)
- `make dev` — run the api on the host; it sources `.env` itself, as does `make seed-admin`
- `make run` — `docker compose up --build`: postgres first, then the api once postgres is *healthy*
- `make test` — unit only
- `make test-integration` — unit + integration (build tag `integration`; **requires `DATABASE_URL`** and fails loudly without it). It does not run against that database — see `internal/dbtest` below
- `make lint` — `go vet` plus staticcheck (pinned v0.8.1; older releases cannot decode Go 1.25+ export data). Both run with `-tags=integration`, or neither would read a single DB-backed test file. staticcheck alone runs under `GOTOOLCHAIN=auto`: its module declares `go 1.26.0`, above go.mod's floor, so the repo-wide `local` pin would break `make lint` on any machine — CI included — running exactly the Go go.mod asks for
- `make docker-build` — production image, prints its size
- `make seed-admin` — idempotent admin from `ADMIN_EMAIL` / `ADMIN_PASSWORD` in `.env`

Single test: `go test ./internal/workflow -run TestTransition -v`. Integration only: `go test -tags=integration ./... -run TestX`.

## Architecture

Layering is package-per-domain under `internal/`, each owning its model, repo (raw pgx, no ORM), and HTTP handlers. `cmd/api/main.go` wires everything and starts the server; it also runs `migrate up` before listening, so a container start is self-contained (this is what makes Render deploys work without a separate migration step).

Two pieces carry the design weight:

**`internal/workflow`** — a pure state machine with no DB or HTTP dependency: a `map[State][]State` transition table and `Transition(from, to State) error` returning `ErrInvalidTransition`. `rejected` and `withdrawn` are terminal. Handlers call `Transition` *before* writing, then update `applications.status` and insert a `status_transitions` audit row **in the same transaction**. Invalid transitions surface as HTTP 409 with `from` and `allowed` (from `workflow.Next(from)`) alongside the error; an unrecognised target state is 400, not 409.

**`internal/auth`** — JWT HS256 issue/verify plus two middlewares: `RequireAuth` (parses bearer token, 401 on missing/invalid/expired, injects a `Principal` into request context) and `RequireRole("admin")` (403 otherwise; **401 when there is no principal at all**, which means the route was mounted without `RequireAuth`). Ownership is enforced at the query level, not just in middleware: a `user` role sees only rows where `user_id` matches their token claim, and a non-owner requesting someone else's application gets **404, not 403** (deliberate — don't leak existence).

Three invariants there that are easy to undo by accident, each pinned by a test that a mutation check confirmed will fail:

- `Verify` passes `jwt.WithValidMethods` so the `alg` header a client sends cannot select the algorithm. What that actually catches is RS256/HS384/HS512 confusion — `alg: none` is separately refused by golang-jwt v5 unless the keyfunc returns its unsafe sentinel. Don't repeat the folk version of this; the comment in `jwt.go` has it right.
- `NewIssuer` rejects a `JWT_SECRET` under 32 bytes (RFC 7518 §3.2). HS256 is only as strong as its key.
- Every auth failure is one identical 401 body, and `auth.DummyCheck` keeps unknown-email login *timing* identical too. Do not add a more helpful message.

`internal/dbtest` provisions the databases the integration suites run against. Every suite opens by dropping the public schema, so two rules hold and both are load bearing:

- **Never the database `DATABASE_URL` names.** `dbtest.Open(ctx, suite)` derives a `jobtracker_test_<suite>` sibling from it — swapping the database name and nothing else, so host, port, credentials and sslmode carry over — and `(*DB).Reset` opens its own connection and asks Postgres `current_database()` before dropping anything. The suite is recorded by `Open` and cannot be supplied, so there is no way to hand `Reset` a `DB` naming the dev database and have it agree. `TestReset_RefusesADatabaseItDidNotName` is what keeps that true.
- **Never a database another suite is using.** `go test ./...` runs one binary per package in parallel, so a shared database means `internal/db`'s four `drop schema public cascade` calls land in the middle of `test/integration`'s run. One database per suite, named by the `Suite*` constants.

Creation holds an advisory lock across the existence check and `CREATE DATABASE`. Postgres holds nothing between those two steps itself, so concurrent callers all pass the check and the losers fail with a unique violation on `pg_database_datname_index` (**23505**, not the `duplicate_database` 42P04 that "already exists" looks like). That is a first-run-only race — it broke CI once and passed everywhere else. `TestOpen_IsSafeWhenSuitesStartTogether` pins it; removing the lock makes it fail with CI's exact error.

`internal/db` owns the pgx pool and migrate runner; `internal/httpx` owns JSON encoding, the `{"error": "..."}` response shape, and `EchoRequestID`, which returns chi's request id in an `X-Request-Id` **header** — not in the body, because the PRD fixes the error shape. `httpx.Decode` caps bodies at 1 MB, rejects unknown fields and trailing content, and returns a `*httpx.Error` carrying the status — so handlers call `httpx.WriteDecodeError(w, err)` and an oversized body is a 413 rather than a blanket 400. All config comes from env vars only (`DATABASE_URL`, `JWT_SECRET`, `PORT`) — no config files.

**Search** — `applications.search_vec` is a *generated stored* tsvector column over role_title/location/notes with a GIN index. Note the two-argument `to_tsvector('english', ...)`: the one-argument form is only STABLE and a generated column requires IMMUTABLE.

`?q=` is a **UNION of two ranked branches**, not one OR'd predicate (PRD §Endpoints): the tsvector branch ranked by `ts_rank`, and a `companies.name ilike` branch at a fixed rank of 1.0, `union all`-ed and deduplicated by `max(rank)` per id. Splitting them is what keeps the tsvector predicate index-eligible — OR-ing an unindexed `ilike` into the same `where` defeats the GIN index, and `EXPLAIN` must still show a Bitmap Index Scan on `applications_search_idx`.

Two planner facts that integration test 12 depends on, both measured rather than assumed, and both worth being able to explain:

- **A GIN index must be vacuumed before its cost looks right.** New entries sit in a pending list until vacuum merges them, and the planner prices a scan over an unmerged list at what reading the list would cost. Same index, same rows: cost **127.56 before `VACUUM`, 12.84 after** — the difference between the planner rejecting `applications_search_idx` and choosing it. `ANALYZE` does not flush it. Any test or benchmark that bulk-inserts and then reasons about a plan has to `vacuum` first.
- **`applications_user_idx` competes with the GIN index on the owner-scoped query**, and which wins turns on term selectivity, not table size. On 2000 rows owned by one user, a term matching 1 row in 10 plans as an Index Scan on `applications_user_idx` with the tsvector predicate as a `Filter`; 1 row in 500 plans as a Bitmap Index Scan on `applications_search_idx`. Both are correct — a term matching a tenth of the table is not what a GIN index is for. Don't "fix" the first case.

**Companies** are a separate normalized table; `POST /applications` upserts by name (`on conflict (name) do update ... returning id`) rather than storing a company string per row.

**Routing** lives in `internal/server/router.go`, not `main.go`, because `package main` can't be imported and a test that rewires its own routes stops testing the ones that ship. Every authenticated route sits inside one `r.Group`, so the default for a new route added in that block is "protected" rather than "public". `middleware.RealIP` is deliberately absent — it trusts a spoofable `X-Forwarded-For` and nothing here keys off the client address. `router_test.go` builds the real router over a nil pool and asserts each protected route 401s **with `WWW-Authenticate: Bearer`**; handlers also refuse a request with no principal, so without that header check a route mounted outside the group would still look like a pass.

## CI/CD

`ci.yml` (`name: CI` — `deploy.yml`'s `workflow_run` matches that string character for character) runs on PRs and pushes to `main`. It calls `make lint` and `make test-integration` rather than open-coding `go vet` and a staticcheck action, deliberately: the Makefile targets carry `-tags=integration`, and a CI step that drops it lints none of the DB-backed tests while still reporting green. Go comes from `go-version-file: go.mod`; Postgres is a `services: postgres:16` container whose health check gates the first step, which is what replaces `make db-up`'s wait loop. `deploy.yml` fires only on a **successful** CI run on `main` and curls `$RENDER_DEPLOY_HOOK_URL`, passed through `env:` so the URL — which is itself the credential — never becomes part of a shell command. Tests should stay under 60s in CI (~16s locally). Render builds the `Dockerfile` (multi-stage golang builder → distroless/static, `CGO_ENABLED=0`; the builder tag must be >= the `go` directive in go.mod); the database is Supabase Postgres via the session pooler with `sslmode=require`.

Compose runs both services. `api` waits on `condition: service_healthy`, reads `.env` optionally (`required: false`, so a fresh clone can still `make db-up` without one) and overrides `DATABASE_URL` and `PORT` inline — inside the network Postgres answers to `postgres`, while `.env` carries the localhost form every host-side target needs.
