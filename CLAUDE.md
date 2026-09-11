# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

The product requirements are in docs/PRD.md. Read it before any change.
The stack, repo layout, schema, and endpoints are fixed. Do not substitute
libraries or add anything listed under "Explicitly out of scope."
Go version is set in go.mod; CI reads it via go-version-file. Deadline is
Thursday 11 AM ET; scope discipline over polish. Run the relevant tests after
every phase and show the output.

## Current state

Phases 1-3 are done: module, migrations, `internal/db`, `/healthz`, Makefile, Dockerfile, postgres-only Compose, `internal/workflow`, and `internal/auth` + `internal/httpx`. Deployed to Render at https://job-tracker-api-8zqe.onrender.com against Supabase.

Phase 4 is built apart from the integration suite: `internal/users`, `internal/applications`, `internal/server/router.go`, the full `cmd/api/main.go`, and DB-free unit tests for search SQL, the JSON date and `Optional[T]`, user validation, and router wiring. `test/integration` is the remaining piece. Phases 5-6 are not started.

**Before Phase 4 deploys:** `JWT_SECRET` on Render must be at least 32 bytes or the container will refuse to start (see `auth.MinSecretLen`).

`docs/PRD.md` is the authoritative spec. `docs/PLAN.md` is the six-phase build order derived from it — it records which files each phase adds, the tests that prove it, and every place the PRD was ambiguous along with the decision taken. Read both before writing anything.

## Scope discipline

This is a resume-artifact MVP with a hard deadline (Thursday Sept 10, 2026, 11:00 AM ET). The PRD has an explicit out-of-scope list (refresh tokens, password reset, rate limiting, OpenAPI docs, frontend, pagination cursors, soft deletes, metrics, sub-resources). Do not build anything not named in the PRD, and do not substitute libraries — the stack is pinned deliberately.

## Commands

- `make db-up` / `make db-down` / `make db-reset` — postgres container (db-reset drops the volume)
- `make migrate` — apply migrations (`go run ./cmd/api -migrate-only`)
- `make dev` — run the api on the host; `make run` (phase 5) will be `docker compose up --build`
- `make test` — unit only
- `make test-integration` — unit + integration (build tag `integration`; **requires `DATABASE_URL`** and fails loudly without it)
- `make lint` — `go vet ./...` plus staticcheck (pinned v0.8.1; older releases cannot decode Go 1.25+ export data)
- `make docker-build` — production image, prints its size
- `make seed-admin` — phase 4; idempotent admin from `ADMIN_EMAIL` / `ADMIN_PASSWORD`

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

`internal/db` owns the pgx pool and migrate runner; `internal/httpx` owns JSON encoding, the `{"error": "..."}` response shape, and `EchoRequestID`, which returns chi's request id in an `X-Request-Id` **header** — not in the body, because the PRD fixes the error shape. `httpx.Decode` caps bodies at 1 MB, rejects unknown fields and trailing content, and returns a `*httpx.Error` carrying the status — so handlers call `httpx.WriteDecodeError(w, err)` and an oversized body is a 413 rather than a blanket 400. All config comes from env vars only (`DATABASE_URL`, `JWT_SECRET`, `PORT`) — no config files.

**Search** — `applications.search_vec` is a *generated stored* tsvector column over role_title/location/notes with a GIN index. Note the two-argument `to_tsvector('english', ...)`: the one-argument form is only STABLE and a generated column requires IMMUTABLE.

`?q=` is a **UNION of two ranked branches**, not one OR'd predicate (PRD §Endpoints): the tsvector branch ranked by `ts_rank`, and a `companies.name ilike` branch at a fixed rank of 1.0, `union all`-ed and deduplicated by `max(rank)` per id. Splitting them is what keeps the tsvector predicate index-eligible — OR-ing an unindexed `ilike` into the same `where` defeats the GIN index, and `EXPLAIN` must still show a Bitmap Index Scan on `applications_search_idx`.

**Companies** are a separate normalized table; `POST /applications` upserts by name (`on conflict (name) do update ... returning id`) rather than storing a company string per row.

**Routing** lives in `internal/server/router.go`, not `main.go`, because `package main` can't be imported and a test that rewires its own routes stops testing the ones that ship. Every authenticated route sits inside one `r.Group`, so the default for a new route added in that block is "protected" rather than "public". `middleware.RealIP` is deliberately absent — it trusts a spoofable `X-Forwarded-For` and nothing here keys off the client address. `router_test.go` builds the real router over a nil pool and asserts each protected route 401s **with `WWW-Authenticate: Bearer`**; handlers also refuse a request with no principal, so without that header check a route mounted outside the group would still look like a pass.

## CI/CD

`ci.yml` runs on PRs and pushes to `main`: vet + staticcheck, then integration tests against a `services: postgres:16` container. `deploy.yml` fires only after CI succeeds on `main` and just curls `$RENDER_DEPLOY_HOOK_URL`. Tests should stay under 60s in CI. Render builds the `Dockerfile` (multi-stage golang builder → distroless/static, `CGO_ENABLED=0`; the builder tag must be >= the `go` directive in go.mod); the database is Supabase Postgres via the pooler with `sslmode=require`.
