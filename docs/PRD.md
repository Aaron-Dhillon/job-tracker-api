# PRD: Job Application Tracker API (MVP)

## Purpose

A RESTful API in Go for tracking job applications. This MVP exists to make the following resume claims true, verbatim, and nothing beyond them:

> Built a RESTful API in Go with JWT authentication, role-based access control, and a state-machine workflow engine, writing clean, maintainable code with automated tests on every pull request.
> Designed a normalized PostgreSQL schema with full-text search, containerized with Docker Compose and deployed to cloud infrastructure via GitHub Actions CI/CD.

Hard deadline: deployed and demonstrable by **Thursday Sept 10, 2026, 11:00 AM ET**. Scope discipline matters more than polish. If a feature is not listed below, do not build it.

## Stack (fixed, do not substitute)

- Go 1.22+, `net/http` with `github.com/go-chi/chi/v5` router
- PostgreSQL 16, accessed via `github.com/jackc/pgx/v5` (no ORM)
- Migrations: plain SQL files applied with `github.com/golang-migrate/migrate/v4`
- JWT: `github.com/golang-jwt/jwt/v5`, HS256
- Passwords: `golang.org/x/crypto/bcrypt`
- Config via environment variables only (`DATABASE_URL`, `JWT_SECRET`, `PORT`)
- Docker + Docker Compose for local dev (api + postgres)
- GitHub Actions for CI (test on every PR) and CD (deploy on merge to `main`)
- Deploy: Render web service (Docker), database on Supabase Postgres
- Tests: standard `testing` package plus `github.com/stretchr/testify`. Integration tests run against a real Postgres via `services:` in GitHub Actions.

## Repo layout

```
job-tracker-api/
  cmd/api/main.go              # wiring, server start
  internal/
    auth/                      # JWT issue/verify, bcrypt, middleware
    users/                     # user model, repo, handlers (register/login)
    applications/              # application model, repo, handlers, search
    workflow/                  # state machine: states, transitions, Transition()
    db/                        # pgx pool, migrate runner
    httpx/                     # JSON helpers, error responses, request ID
  migrations/
    0001_init.up.sql / .down.sql
  docker-compose.yml
  Dockerfile
  .github/workflows/ci.yml
  .github/workflows/deploy.yml
  Makefile                     # run, test, migrate, lint
  README.md
```

## Data model (normalized)

```sql
users (
  id            uuid primary key default gen_random_uuid(),
  email         text not null unique,
  password_hash text not null,
  role          text not null check (role in ('user','admin')),
  created_at    timestamptz not null default now()
)

companies (
  id         uuid primary key default gen_random_uuid(),
  name       text not null unique,
  created_at timestamptz not null default now()
)

applications (
  id           uuid primary key default gen_random_uuid(),
  user_id      uuid not null references users(id) on delete cascade,
  company_id   uuid not null references companies(id),
  role_title   text not null,
  location     text,
  notes        text,
  status       text not null default 'applied'
               check (status in ('applied','screening','interviewing','offer','rejected','withdrawn')),
  applied_on   date not null default current_date,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now(),
  search_vec   tsvector generated always as (
                 to_tsvector('english', coalesce(role_title,'') || ' ' || coalesce(location,'') || ' ' || coalesce(notes,''))
               ) stored
)
create index applications_search_idx on applications using gin (search_vec);
create index applications_user_idx on applications(user_id);

status_transitions (
  id          bigserial primary key,
  application_id uuid not null references applications(id) on delete cascade,
  from_status text not null,
  to_status   text not null,
  changed_by  uuid not null references users(id) on delete cascade,
  changed_at  timestamptz not null default now()
)
```

Normalization note for interview: companies are a separate table so the same company across many applications is one row; transitions are an append-only audit table rather than columns on applications.

## State machine (`internal/workflow`)

States: `applied`, `screening`, `interviewing`, `offer`, `rejected`, `withdrawn`.

Allowed transitions (a transition table, a `map[State][]State`):

```
applied      -> screening, rejected, withdrawn
screening    -> interviewing, rejected, withdrawn
interviewing -> offer, rejected, withdrawn
offer        -> rejected, withdrawn      (accepting is out of scope)
rejected     -> (terminal)
withdrawn    -> (terminal)
```

`Transition(from, to State) error` returns `ErrInvalidTransition` if `to` is not in the allowed list for `from`. The handler calls this before writing; on success it updates `applications.status` and inserts a `status_transitions` row in the same transaction. Unit tests cover every allowed transition and at least three disallowed ones, including from a terminal state.

## Auth and RBAC (`internal/auth`)

- `POST /auth/register` `{email, password}` creates a `user` role account. Bcrypt cost 12.
- `POST /auth/login` returns `{token}`; JWT HS256, 24h expiry, claims: `sub` (user id), `role`, `exp`, `iat`.
- Middleware `RequireAuth` parses `Authorization: Bearer <token>`, rejects missing/invalid/expired with 401, injects user id and role into context.
- Middleware `RequireRole("admin")` returns 403 for non-admins.
- Ownership rule: a `user` can only read/modify applications where `user_id` matches their token. An `admin` can read any application and list all users.
- Admin creation: make seed-admin target reading ADMIN_EMAIL and ADMIN_PASSWORD from env, idempotent.

## Endpoints

All JSON. Errors are `{"error": "message"}` with appropriate status.

```
POST   /auth/register
POST   /auth/login

GET    /applications                 list own (admin: all), supports ?status=&q=&limit=&offset=
POST   /applications                 create {company_name, role_title, location?, notes?, applied_on?}
GET    /applications/{id}
PATCH  /applications/{id}            update role_title/location/notes/applied_on only (not status)
DELETE /applications/{id}
POST   /applications/{id}/transition {to: "screening"}   -> 409 on invalid transition
GET    /applications/{id}/history    list status_transitions for the application

GET    /admin/users                  admin only
GET    /healthz                      no auth, returns {"status":"ok"} and checks DB ping
```

`?q=` performs full-text search as a **UNION of two ranked branches**, not a single OR'd predicate:

1. **Text branch** — `where search_vec @@ plainto_tsquery('english', $1)`, ranked by `ts_rank(search_vec, plainto_tsquery('english', $1))`.
2. **Company branch** — `join companies c ... where c.name ilike '%' || $1 || '%'`, assigned a fixed rank of `1.0`.

The branches are `union all`-ed, deduplicated by `max(rank)` per application id, then ordered `rank desc, applied_on desc, id`. A row matching both branches keeps the higher score. The fixed `1.0` deliberately outranks typical `ts_rank` values (~0.01-0.1), so an exact company match sorts above a body-text match — searching "Visa" surfaces applications *at* Visa ahead of ones that merely mention Visa in the notes.

Splitting the branches (rather than OR-ing them in one `where`) is what keeps the tsvector predicate index-eligible: Postgres plans branch 1 as a Bitmap Index Scan on `applications_search_idx`, which an OR against a non-indexed `ilike` would defeat.

`POST /applications` upserts the company by name (`insert ... on conflict (name) do update set name = excluded.name returning id`).

## Testing

- Unit: `workflow` transition table (table-driven), JWT issue/verify, bcrypt round trip, middleware behaviour with fake handlers.
- Integration (build tag `integration`): spin up handlers against a real Postgres (`DATABASE_URL` from CI service container), run migrations, then: register → login → create → transition happy path → invalid transition returns 409 → search by `?q=` returns the right row → non-owner gets 404 → admin sees all.
- `make test` runs unit; `make test-integration` runs both.
- Target: tests finish under 60s in CI.

## CI/CD

`.github/workflows/ci.yml` — on `pull_request` and `push` to `main`:
1. checkout, setup-go, cache modules
2. `go vet ./...` and `staticcheck` (or `golangci-lint` if trivial to add)
3. `services: postgres:16` with health check
4. run migrations, `make test-integration`

`.github/workflows/deploy.yml` — on `push` to `main` after CI succeeds (`workflow_run` or a job with `needs`):
1. `curl -X POST "$RENDER_DEPLOY_HOOK_URL"` (stored as a repo secret)

Render service is Docker-based, builds from `Dockerfile`, env vars `DATABASE_URL` (Supabase connection string, pooler, sslmode=require), `JWT_SECRET`, `PORT=8080`. Migrations run on container start (`main.go` calls migrate up before listening) so a deploy is self-contained.

## Docker

- `Dockerfile`: multi-stage, `golang:1.26` builder → `gcr.io/distroless/static` runtime, static binary (`CGO_ENABLED=0`). Final image well under 30 MB. (The builder must be at least the `go` directive in `go.mod`, which `golang-migrate/v4` pins to 1.25.11 — still inside the "Go 1.22+" floor above.)
- `docker-compose.yml`: `api` (build .) + `postgres:16` with a named volume; `api` depends_on postgres healthcheck. `make run` = `docker compose up --build`.

## README (must exist, short)

- One-paragraph description
- Quickstart: `cp .env.example .env`, `make run`, then three curl commands: register, login, create application
- Endpoint table
- State diagram (ASCII is fine)
- Live URL of the deployed `/healthz`

## Definition of done (check every box before noon Thursday)

- [ ] `make run` brings up api + postgres locally and `/healthz` returns ok
- [ ] Register/login/create/transition/search all work via curl locally
- [ ] Invalid transition returns 409; terminal states cannot transition
- [ ] Non-owner cannot read another user's application; admin can
- [ ] `?q=` full-text search returns ranked results and uses the GIN index (`explain` shows Bitmap Index Scan)
- [ ] CI is green on a PR and on `main`
- [ ] Merge to `main` triggers a Render deploy; public `/healthz` URL works
- [ ] README is accurate; repo is public on GitHub
- [ ] Aaron can explain, without notes: the transition table, the JWT middleware, the tsvector/GIN setup, and what the CI pipeline does step by step

## Explicitly out of scope

Refresh tokens, email verification, password reset, pagination cursors, rate limiting, OpenAPI docs, frontend, interviews/contacts sub-resources, soft deletes, metrics, multi-tenant orgs. If any of these get built, the deadline slips.
