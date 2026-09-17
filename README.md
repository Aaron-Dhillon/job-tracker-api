# Job Application Tracker API

A REST API for tracking job applications through a hiring pipeline, written in Go
with no ORM and no framework beyond a router. Accounts are JWT-authenticated,
every application belongs to exactly one user, and status changes are validated
against a state machine and recorded in an append-only audit trail. Full-text
search over role, location and notes runs on a generated `tsvector` column with
a GIN index. Stack: Go 1.25, chi, pgx, golang-migrate, PostgreSQL 16, Docker.

## Live

Deployed on Render against Supabase Postgres; every green CI run on `main`
redeploys it.

```bash
curl -s https://job-tracker-api-8zqe.onrender.com/healthz
# {"status":"ok"}
```

`/healthz` pings the database, so a `200` means the whole service is up, not
just the process.

## Quickstart

```bash
cp .env.example .env
make run          # builds the api image, starts it behind a healthy Postgres
```

`make run` waits for Postgres's health check before starting the API, which
applies migrations itself on boot — there is no separate migration step.

```bash
# 1. register
curl -s -X POST localhost:8080/auth/register \
  -H 'content-type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse-battery-staple"}'

# 2. log in and keep the token
TOKEN=$(curl -s -X POST localhost:8080/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse-battery-staple"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

# 3. create an application
curl -s -X POST localhost:8080/applications \
  -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"company_name":"Acme","role_title":"Platform Engineer",
       "location":"Remote","notes":"kubernetes and postgres at scale",
       "applied_on":"2026-09-11"}'
```

Then `curl -s "localhost:8080/applications?q=kubernetes" -H "authorization: Bearer $TOKEN"`.

`make dev` runs the API on the host instead, against the Postgres from
`make db-up`; it reads `.env` itself. `make help` lists every target.

**`JWT_SECRET` must be at least 32 bytes** or the process refuses to start —
HS256 is only as strong as its key. `openssl rand -base64 32` produces one.

## Endpoints

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| `GET` | `/healthz` | — | `200` `{"status":"ok"}`; `503` when the database ping fails, not a hardcoded ok |
| `POST` | `/auth/register` | — | `201` with the user, no token; `400` for an invalid email or a password outside 8–72 bytes; `409` if the email is taken |
| `POST` | `/auth/login` | — | `200` `{"token": "…"}` (24h); one identical `401` for a bad email or a bad password |
| `GET` | `/applications` | bearer | your own, or every row for an admin; `?q=` `?status=` `?limit=` (default 20, capped at 100) `?offset=` |
| `POST` | `/applications` | bearer | `company_name` and `role_title` required; upserts the company by name; `applied_on` defaults to today; `201` |
| `GET` | `/applications/{id}` | bearer | owner, or any row for an admin |
| `PATCH` | `/applications/{id}` | bearer | owner only; `role_title` `location` `notes` `applied_on`; `status` in the body is a `400` |
| `DELETE` | `/applications/{id}` | bearer | owner only; `204`, history cascades |
| `POST` | `/applications/{id}/transition` | bearer | owner only; `{"to": "screening"}`; `409` on an illegal move, `400` for a state that does not exist |
| `GET` | `/applications/{id}/history` | bearer | owner, or any row for an admin |
| `GET` | `/admin/users` | admin | `403` for a non-admin token; never returns password hashes |

Errors are always `{"error": "…"}`. A `409` from a transition carries two extra
fields so a client can render the next steps without hardcoding the machine:

```json
{"error":"invalid transition","from":"rejected","allowed":[]}
```

Requesting an application you do not own returns **404, not 403** — a 403 would
confirm the row exists. The same goes for an admin's `PATCH`, `DELETE` or
transition on someone else's row: admin reads everything and writes only its
own. A missing or invalid token is a `401` with `WWW-Authenticate: Bearer`;
unknown JSON fields are a `400` and bodies over 1 MB a `413`.

## Status workflow

```
   applied ---> screening ---> interviewing ---> offer
      |             |               |              |
      +-------------+-------+-------+--------------+
                            |
                 +----------+----------+
                 v                     v
             rejected              withdrawn
            (terminal)             (terminal)
```

Every non-terminal state can be rejected or withdrawn; nothing leaves `rejected`
or `withdrawn`; no state transitions to itself. The transition is checked, the
status updated and the audit row inserted inside one transaction, behind a
`SELECT … FOR UPDATE` on the application — two concurrent requests cannot both
pass the check.

## Design notes

- **Search is a union, not an `OR`.** `?q=` runs two ranked branches — the
  tsvector predicate ranked by `ts_rank`, and a `companies.name ILIKE` branch at
  a fixed rank of 1.0 — `UNION ALL`-ed and deduplicated by `max(rank)`. OR-ing
  an unindexed `ILIKE` into the same `WHERE` would force a scan of every row and
  defeat the GIN index; splitting them keeps the tsvector branch index-eligible,
  and an integration test `EXPLAIN`s the exact query the API builds to prove it.
  The company branch has no index and never will: it scans a small table, which
  is honest about what it is rather than pretending to be free.
- **Ownership is enforced in the query, not just in middleware.** A non-admin's
  reads, and every write, carry `user_id` in the `WHERE` clause, so someone
  else's row is simply not found. Admin is deliberately **read-only** on other
  people's applications.
- **Migrations are embedded** (`go:embed` + golang-migrate's `iofs`), so the
  distroless image ships them and a container start is self-contained. That is
  what makes a Render deploy work with no migration step.
- **Each integration suite owns its own database.** They drop and recreate the
  public schema, so every one derives a `jobtracker_test_<suite>` sibling from
  `DATABASE_URL` and creates it on the fly (`internal/dbtest`), and refuses to
  reset anything else. A test run never touches the database you were working
  in — and because `go test ./...` runs package binaries in parallel, never
  drops a schema another suite is mid-run against either.

## Development

| | |
| --- | --- |
| `make db-up` / `make db-down` / `make db-reset` | Postgres container; `db-reset` drops the volume |
| `make run` | API and Postgres in Docker Compose |
| `make dev` | run the API on the host, loading `.env` |
| `make seed-admin` | create or update the admin from `ADMIN_EMAIL` / `ADMIN_PASSWORD`; the API also does this on boot when both are set |
| `make test` | unit tests, no database |
| `make test-integration` | unit + integration, against a real Postgres |
| `make lint` | `go vet` plus staticcheck, both with `-tags=integration` |
| `make docker-build` | the production image, and prints its size |

CI runs lint and the integration suite against a `postgres:16` service on every
pull request and every push to `main`; a green run on `main` triggers the Render
deploy hook.
