# Build Plan — Job Application Tracker API

Execution order for `docs/PRD.md`. The PRD is the spec; this is the sequence. Every phase ends with a command that proves it works. If a phase's verify command doesn't pass, do not start the next one.

**Deadline: Thu Sept 10 2026, 11:00 AM ET.** Estimated ~8.5h of build. Phase 6 depends on third-party infra (Supabase, Render) that can fail in ways code review can't catch — see the de-risking note at the end of Phase 1.

## Before you start (~15 min)

| # | Action | Status |
|---|---|---|
| 1 | `sudo xcode-select --switch /Library/Developer/CommandLineTools` — `/usr/bin/make` was shimming through a crashing `xcodebuild` | **Done.** GNU Make 3.81 responds |
| 2 | `git branch -m master main`, push, repoint the GitHub default | **Done.** On `main` |
| 3 | Docker Desktop running | **Done.** Engine 29.7.2 |

Locked environment facts: Go 1.27.1 local · Docker 29.7.2 / Compose v5.5.1 · GNU Make **3.81** (avoid `.ONESHELL`, `$(file ...)`, and other Make 4.x features) · no host `psql` (use `docker compose exec postgres psql`) · no `migrate` CLI (migrations run from Go via embedded SQL) · repo already **public** · `gh` authenticated as `Aaron-Dhillon`.

---

## Phase 1 — Schema, migrations, `internal/db`, Dockerfile (~90 min)

Establishes the module, the database, and a running `/healthz`. Everything downstream tests against this.

> **Deviation from the requested split:** the `Dockerfile` and `.dockerignore` are built here, not in Phase 5, so the `/healthz` skeleton can be deployed to Render before Phase 2 starts. Phase 5 keeps the Compose `api` service.

**Files**

| File | Contents |
|---|---|
| `go.mod` | `module github.com/Aaron-Dhillon/job-tracker-api`. **Resolved to `go 1.25.11`** — `golang-migrate/v4 v4.20.1` requires it, which sets the floor for the module. Still satisfies the PRD's "Go 1.22+". No `toolchain` line (`GOTOOLCHAIN=local` everywhere). |
| `.gitignore`, `.env.example` | `.env`, `bin/`. `.env.example` carries `DATABASE_URL`, `JWT_SECRET`, `PORT`, `ADMIN_EMAIL`, `ADMIN_PASSWORD` — see the two-URL note in Phase 5. |
| `migrations/0001_init.up.sql` | Schema exactly as PRD §Data model, plus the four amendments below. |
| `migrations/0001_init.down.sql` | `drop table ... cascade` in reverse dependency order. |
| `migrations/embed.go` | `package migrations` + `//go:embed *.sql` → `var FS embed.FS`. Embedding (not a copied directory) is what lets the distroless image ship migrations. |
| `internal/db/pool.go` | `New(ctx, url) (*pgxpool.Pool, error)`, sane `MaxConns`, `Ping(ctx)`. |
| `internal/db/migrate.go` | `Up(url string) error` — `iofs.New(migrations.FS, ".")` → `migrate.NewWithSourceInstance`. Treat `migrate.ErrNoChange` as success. |
| `cmd/api/main.go` | Skeleton only: env → pool → `db.Up` → chi router with `GET /healthz` → `ListenAndServe`. Grown in Phase 4. |
| `docker-compose.yml` | **postgres service only** (`postgres:16`, `pg_isready` healthcheck, named volume `pgdata`). The `api` service arrives in Phase 5. |
| `Dockerfile`, `.dockerignore` | **Pulled forward from Phase 5.** Multi-stage `golang:1.26-bookworm` → `gcr.io/distroless/static:nonroot`, `CGO_ENABLED=0`. Migrations are embedded, so nothing else is copied. |
| `Makefile` | `db-up`, `db-down`, `migrate`, `dev`, `test`, `test-integration`, `lint`, `fmt`, `tidy`, `build`, `docker-build`. Grown in Phase 4. |

**Four amendments to the PRD schema** (each is a fix, not an embellishment):

- `to_tsvector('english', ...)` — the **two-arg** form is required. Single-arg `to_tsvector(text)` is only `stable`, and a generated column demands `immutable`; the migration will be rejected otherwise.
- **`status_transitions.changed_by` gets `on delete cascade`.** Without it, deleting a user with authored transitions raises an FK violation and breaks integration-test teardown. **Decided — `docs/PRD.md` §Data model now carries it.**
- Add `create index applications_company_idx on applications(company_id);` — every list/search query joins on it.
- Add `create index status_transitions_app_idx on status_transitions(application_id, changed_at);` — backs `GET /{id}/history`.

`gen_random_uuid()` is core in PG 13+; **no `pgcrypto` extension** (creating extensions fails on Supabase's pooler anyway).

**Tests that prove it** — `internal/db/migrate_test.go`, build tag `integration`:

- `TestMigrateUp_CreatesSchema` — all four tables exist; `applications_search_idx` present in `pg_indexes` and is `gin`.
- `TestMigrateUp_GeneratedColumn` — insert a row, assert `search_vec` is populated and matches `plainto_tsquery('english', 'engineer')`.
- `TestMigrateUp_StatusCheck` — inserting `status = 'bogus'` returns a check-constraint violation.
- `TestMigrate_UpDownUp` — down then up again succeeds (proves the down migration is real).

**Verify:** `make db-up && make migrate && make test-integration && make docker-build`

**Done when:** `docker compose exec postgres psql -U postgres -d jobtracker -c '\d applications'` shows `search_vec` as `generated always as ... stored`, and `go run ./cmd/api` answers `curl localhost:8080/healthz` with `{"status":"ok"}`.

> **De-risk the deploy now, not at hour 8.** With the Dockerfile in hand, do the Render dry run before Phase 2: create the Supabase project, grab the **session pooler** URI (:5432, `sslmode=require`), point a Render web service at this repo's Dockerfile with `DATABASE_URL`/`JWT_SECRET`/`PORT=8080`, confirm the public `/healthz` returns ok. ~30 min, and it moves the single least-controllable risk to the start of the day. It also proves migrate-on-start works against Supabase, which is the part most likely to surprise you.

---

## Phase 2 — `internal/workflow` (~30 min)

Pure state machine. No database, no HTTP, no imports beyond `errors`. This is the package you'll be asked to explain in interviews, so keep it small enough to read aloud.

**Files:** `internal/workflow/workflow.go`, `internal/workflow/workflow_test.go`

```go
type State string
const (StateApplied State = "applied"; /* ... */)
var allowed = map[State][]State{ /* the PRD table, verbatim */ }
var ErrInvalidTransition = errors.New("invalid transition")
var ErrUnknownState      = errors.New("unknown state")

func Transition(from, to State) error   // signature per PRD — error only
func Valid(s State) bool
func IsTerminal(s State) bool           // len(allowed[s]) == 0
func Next(s State) []State              // for 409 error bodies and the README diagram
```

**Tests that prove it** — table-driven over the **full 6×6 matrix**, all 36 pairs asserted against the expected-allowed set. That covers all 11 permitted transitions and 25 rejections, including every terminal origin — comfortably past the PRD's "at least three disallowed".

Plus: `errors.Is(err, ErrInvalidTransition)` holds; unknown states on either side yield `ErrUnknownState`; self-transitions (`applied → applied`) are rejected; and a guard test asserting every state is a key in `allowed` and every value is a valid state (catches a typo'd table).

**Verify:** `make test`

**Done when:** `go test -cover ./internal/workflow` reports **100.0%**.

`Next` returns a non-nil empty slice for a terminal state and nil only for an unknown one, so the 409 body's `allowed` field marshals as `[]` rather than `null` — which is the commonest 409, since transitioning out of `rejected` or `withdrawn` is what users try. `TestNext_MarshalsForErrorBody` pins the exact JSON.

---

## Phase 3 — `internal/auth` + `internal/httpx` (~60 min)

> **Deviation from the requested split:** `internal/httpx/json.go` is built here, not in Phase 4. `RequireAuth` has to emit `{"error": ...}` on 401, so the writer must exist first.

**Files**

| File | Contents |
|---|---|
| `internal/httpx/json.go` | `WriteJSON(w, status, v)`, `WriteError(w, status, msg)`, `Decode(w, r, &v) error` wrapping `http.MaxBytesReader` (1 MB) + `DisallowUnknownFields`. |
| `internal/auth/password.go` | `HashPassword` (bcrypt **cost 12**), `CheckPassword`. |
| `internal/auth/jwt.go` | `Claims{jwt.RegisteredClaims; Role string}`; `Issuer{secret, ttl}` with `Issue(userID uuid.UUID, role string)` and `Verify(tok string) (Principal, error)`. `sub` = user UUID, 24h expiry, `iat`. `NewIssuer` rejects a secret under `MinSecretLen` (32). |
| `internal/auth/context.go` | Unexported `ctxKey`; `Principal{ID uuid.UUID; Role string}`; `WithPrincipal` / `PrincipalFrom`; `RoleUser`/`RoleAdmin`/`ValidRole`. |
| `internal/auth/middleware.go` | `RequireAuth(v Verifier)`, `RequireRole(role string)`. `Verifier` is the one-method interface `Issuer` satisfies, so the middleware is testable with a stub. |

Four things to get right, all worth being able to explain:

- `Verify` must pass **`jwt.WithValidMethods([]string{"HS256"})`**. Without it the parser honours the `alg` header the *client* chose. The live risk is `RS256`: the library hands our HMAC secret to the RSA verifier as a public key, and a public key is not a secret. **A mutation test settled what this actually defends:** removing `WithValidMethods` is caught by `TestVerify_RejectsOtherAlgorithms` (HS384/HS512), *not* by the `alg: none` test — golang-jwt v5 separately refuses the `none` method unless the keyfunc returns its `UnsafeAllowNoneSignatureType` sentinel. Both tests stay; the allowlist is the defence we own, the library guard is one we inherit.
- **`JWT_SECRET` must be ≥ 32 bytes** (`MinSecretLen`), enforced at `NewIssuer`. RFC 7518 §3.2 requires an HMAC key at least as long as the hash output; a short secret is brute-forceable offline from one captured token, and then anyone mints an admin. `.env.example` was updated — **the Render env var must be rotated to a 32-byte value before Phase 4 deploys**, or the container will refuse to start.
- Login returns one identical 401 for unknown-email and wrong-password. Never leak which — including in *timing*. `auth.DummyCheck` runs a real bcrypt comparison against a throwaway hash when no user matched, so an unknown email costs the same ~250ms as a known one. Identical bodies with a 250ms-versus-0ms split still answers "is this address registered?".
- `RequireRole` answers **401, not 403**, when there is no principal at all: that means the route was mounted without `RequireAuth`, and 403 would claim the caller was identified and rejected.

Note `bcrypt` errors on passwords over 72 bytes (`ErrPasswordTooLong`) rather than truncating — the register handler maps that to 400 in Phase 4.

**Tests that prove it**

- `password_test.go` — hash→check round trip; wrong password fails; `bcrypt.Cost(hash) == 12`; >72-byte password returns an error.
- `jwt_test.go` — issue→verify round trip preserves `sub` and `role`; expired token rejected; wrong secret rejected; **`alg: none` token rejected**; tampered payload rejected.
- `middleware_test.go` — against a fake next-handler: no header → 401; `Authorization: Basic ...` → 401; `Bearer` with garbage → 401; expired → 401; valid → 200 **and** the principal is readable from context. `RequireRole("admin")`: user → 403, admin → 200, no principal → 401.
- `internal/httpx/json_test.go` — error shape is exactly `{"error":"..."}`; unknown fields rejected; oversized body rejected.

**Verify:** `make test` (unit only — still no DB dependency)

**Done when:** `go test ./internal/auth/... ./internal/httpx/...` passes and `make lint` is clean.

**Done.** `auth` 98.7% coverage, `httpx` 97.7%; the uncovered remainder is the HMAC signing-failure branch and the `decodeError` fallback, neither reachable with a valid key and a real `*http.Request`. Five mutations were caught: dropped `WithValidMethods`, cost 12→10, `RequireRole` 401→403 with no principal, dropped `DisallowUnknownFields`, and leaking the verifier's reason into the 401 body. `cmd/api/main.go` now uses `httpx` and the placeholder `writeJSON` is gone.

`Decode` returns `*httpx.Error` carrying a status, so an oversized body is **413** and everything else 400; handlers call `httpx.WriteDecodeError(w, err)` rather than hardcoding 400. The request-id echo named in Phase 4 stays in Phase 4 — it needs the router to be testable end to end.

---

## Phase 4 — users, applications, router, integration tests, Makefile (~3h)

The bulk of the work. Everything before this was foundations.

**Files**

| File | Contents |
|---|---|
| `internal/users/model.go` | `User` (never serialise `password_hash`). |
| `internal/users/validate.go` | `NormalizeEmail` (lowercase + trim), `ValidateCredentials`. `mail.ParseAddress` also accepts `"A" <a@b.c>`, so the parsed `addr.Address` is compared back against the input to reject display-name form. |
| `internal/users/repo.go` | `Create` (pg error `23505` → `ErrEmailTaken`), `Upsert`, `GetByEmail`, `GetByID`, `List`. |
| `internal/users/admin.go` | `EnsureAdmin(ctx, repo, email, pw)` — idempotent upsert, forces `role='admin'`, refreshes the hash so a rotated `ADMIN_PASSWORD` takes effect. **One implementation, two callers** (startup + `make seed-admin`). |
| `internal/users/handlers.go` | `Register`, `Login`, `AdminListUsers`. |
| `internal/applications/model.go` | `Application`, `Transition`, `CreateInput`, `PatchInput`, and `Date` (a `time.Time` that marshals as `YYYY-MM-DD` and rejects anything else). `PatchInput` uses a generic `Optional[T]{Set bool; Value *T}` — see the locked decision below. |
| `internal/applications/repo.go` | `Create` (tx: company upsert → insert), `Get`, `List`, `Update`, `Delete`, `Transition` (tx), `History`. |
| `internal/applications/search.go` | `ParseListParams` + `BuildListQuery`. Exported so integration test 12 can `EXPLAIN` the *real* query rather than a copy. |
| `internal/applications/handlers.go` | All seven `/applications` routes. The transition handler maps `workflow.ErrUnknownState` → 400 and `workflow.ErrInvalidTransition` → 409, and the 409 body carries `from` plus `workflow.Next(from)` as `allowed` (PRD §Endpoints). |
| `internal/server/router.go` | **New file, not in the PRD layout.** `New(deps) http.Handler` — chi router, all routes, middleware stack. Exists so integration tests build the *same* router `main.go` serves; `package main` can't be imported, and duplicated wiring drifts. |
| `cmd/api/main.go` | Full wiring: env → migrate → `NewIssuer` → pool → `EnsureAdmin` if env set → `server.New` → serve with SIGTERM drain. The issuer is built **before** the pool so a short `JWT_SECRET` fails at startup, not at the first login. |
| `internal/httpx/requestid.go` | `EchoRequestID` — copies chi's request id into an `X-Request-Id` **header**. Not into the body: the PRD fixes the error shape as `{"error": "..."}`. |
| `Makefile` | Complete: add `run`, `seed-admin`. |
| Unit tests | `internal/applications/{search,model}_test.go`, `internal/users/users_test.go`, `internal/server/router_test.go`. All DB-free. |
| `test/integration/api_test.go` + `helpers_test.go` | Build tag `integration`. |

Reuse chi's own `middleware.RequestID` and `middleware.Recoverer` rather than writing them; `httpx` only needs a helper to echo the request id back.

**`middleware.RealIP` is deliberately left out.** It rewrites `RemoteAddr` from a client-supplied `X-Forwarded-For`, which anyone can forge, and nothing here keys off the client address — rate limiting is out of scope. Adding it would mean trusting a spoofable header for no gain.

**Two implementation details that decide whether the DoD passes:**

- **Build the search SQL dynamically.** Assemble `[]string` conditions + `[]any` args in `search.go`. Do *not* write one static query guarded by `($3::text is null or ...)` — Postgres can't use the GIN index through a parameterised null guard, and the DoD requires `EXPLAIN` to show a Bitmap Index Scan.
- **`Transition` locks the row.** Inside the tx: `select status ... for update` → `workflow.Transition(from, to)` → `update applications set status, updated_at = now()` → `insert into status_transitions`. The `FOR UPDATE` is what stops two concurrent requests both passing the check and double-transitioning. `updated_at` is set explicitly in every UPDATE — there is no trigger.

Ownership: non-admins get `user_id = $n` appended to the list query; single-item routes compare `app.user_id` to the principal and return **404, not 403** (never confirm that someone else's id exists). Admin is **read-only** on others' rows — `GET` list/item/history only; `PATCH`, `DELETE`, and `POST /transition` still require ownership.

**Tests that prove it** — `test/integration`, real Postgres, `httptest.Server` over `server.New`. Harness: `truncate ... restart identity cascade` in `t.Cleanup`, unique emails per test, **no `t.Parallel()`** (shared database).

| # | Scenario | Asserts |
|---|---|---|
| 1 | `GET /healthz` | 200 `{"status":"ok"}` |
| 2 | Register | 201; duplicate email → 409; bad email / short password → 400 |
| 3 | Login | 200 + token; wrong password → 401 (identical body to unknown email) |
| 4 | Create ×2, same company | both 201; `companies` holds exactly **one** row (proves the upsert) |
| 5 | Read | owner 200; **non-owner 404**; no token 401 |
| 6 | Transition | `applied→screening` 200; history has 1 row; **`screening→applied` 409**; from `rejected` → 409. The 409 body matches `{"error":"invalid transition","from":...,"allowed":[...]}`, with `allowed` an empty array (not null) from a terminal state; `{"to":"bogus"}` → **400**, not 409 |
| 7 | PATCH | updates `role_title`, bumps `updated_at`; **`status` in body → 400** |
| 8 | Search | `?q=` matches on notes/title, top-ranked row correct; `?q=Visa` matches via company name |
| 9 | Filters | `?status=` filters; `?limit=`/`?offset=` page correctly |
| 10 | Delete | 204; subsequent GET 404; transition rows gone (cascade) |
| 11 | Admin | sees all users' applications; `GET /admin/users` 200 without hashes; **non-admin → 403** |
| 12 | **Index proof** | `set local enable_seqscan = off; explain <search query>` output contains `Bitmap Index Scan on applications_search_idx` |

Test 12 turns a manual DoD checkbox into something CI enforces. `enable_seqscan = off` is needed because on a ten-row table the planner will rightly prefer a sequential scan; the test proves the index is *usable*, which is the actual claim.

**Verify:** `make test-integration`

**Done when:** all 12 pass in under 60s, and the README's three curl commands (register → login → create) work against `go run ./cmd/api`.

**Checkpoint (handlers + router).** Everything except `test/integration` is built and green: `make test` passes all seven packages, `make lint` is clean. `router_test.go` proves the wiring with no database — it builds the real router over a nil pool and asserts every one of the eight protected routes answers 401 **with `WWW-Authenticate: Bearer`**. The header matters: each handler *also* refuses a request with no principal, so status alone would still read 401 for a route mounted outside the auth group. Four mutations confirmed the teeth: a route moved out of the group, the admin group gated on `user` instead of `admin`, `EchoRequestID` dropped, and the JSON 405 handler dropped.

---

## Phase 5 — Dockerfile + Compose (~45 min)

**Files:** `docker-compose.yml` (add the `api` service). The `Dockerfile` and `.dockerignore` landed in Phase 1.

- Compose: `api` builds `.`, `depends_on: postgres: {condition: service_healthy}`, `env_file: .env`.
- `make run` becomes `docker compose up --build` (Phase 1's `make dev` stays as the no-container path).

> **The two-URL trap.** Inside Compose the host is `postgres:5432`; from your shell it's `localhost:5432`. Set `DATABASE_URL` **inline on the `api` service** in `docker-compose.yml` (host `postgres`), and let `.env` hold the `localhost` form used by `make migrate` and `make test-integration`. `.env.example` should show both, commented.

**Tests that prove it:** no new Go tests — this phase is verified by running it.

**Verify:** `make run`

**Done when:** `curl -s localhost:8080/healthz` returns ok against the *containerised* API; `docker images job-tracker-api --format '{{.Size}}'` is **< 30 MB**; and the full curl chain (register → login → create → transition → `?q=` search) works against the container.

---

## Phase 6 — CI/CD + README + deploy (~90 min + ~45 min manual infra)

**Files:** `.github/workflows/ci.yml`, `.github/workflows/deploy.yml`, `README.md`

**`ci.yml`** — `on: pull_request` + `push: branches: [main]`. One `test` job:

1. `actions/checkout@v4`
2. `actions/setup-go@v5` with `go-version-file: go.mod`, `cache: true`
3. `services: postgres:16` — `--health-cmd pg_isready --health-interval 10s --health-timeout 5s --health-retries 5`
4. `go vet ./...`
5. staticcheck **v0.8.1** (older releases cannot decode Go 1.25+ export data — 2025.1.1 fails outright with `export data version 4 is greater than maximum supported version 2`). Pin it; an unpinned `latest` will eventually fail a build you didn't change
6. `make test-integration` with `DATABASE_URL=postgres://postgres:postgres@localhost:5432/jobtracker?sslmode=disable`, `JWT_SECRET=test-secret`

No separate migrate step is needed — the test harness calls `db.Up`. (The PRD lists one; harmless either way.) The GNU Make 3.81 constraint is local-only; `ubuntu-latest` ships Make 4.x.

**`deploy.yml`** — `on: workflow_run: {workflows: ["CI"], types: [completed], branches: [main]}`, guarded by `if: github.event.workflow_run.conclusion == 'success'`, one step: `curl -fsS -X POST "${{ secrets.RENDER_DEPLOY_HOOK_URL }}"`.

> Two `workflow_run` gotchas that cost an hour each: the `workflows: ["CI"]` string must match `ci.yml`'s `name:` **exactly**, and `workflow_run` only fires for a workflow file that already exists **on the default branch** — so the first merge to `main` lands `deploy.yml` without triggering it. The second merge is the first real deploy. Don't debug a phantom.

**Manual infra checklist** (do the Phase-1 dry run first and most of this is already done):

- [ ] Supabase project; copy the **session pooler** URI (:5432) with `sslmode=require` — *not* the transaction pooler (:6543), which breaks golang-migrate's advisory lock and pgx's statement cache
- [ ] Render web service, Docker runtime, health check path `/healthz`
- [ ] Render env: `DATABASE_URL`, `JWT_SECRET`, `PORT=8080`, `ADMIN_EMAIL`, `ADMIN_PASSWORD`
- [ ] GitHub repo secret `RENDER_DEPLOY_HOOK_URL`
- [x] Repo is public

**`README.md`** — one-paragraph description; quickstart (`cp .env.example .env`, `make run`, three curl commands); endpoint table; ASCII state diagram; live `/healthz` URL.

**Verify:** open a PR → CI green; merge → deploy fires → `curl https://<service>.onrender.com/healthz`

**Done when:** every box in PRD §Definition of done is ticked.

---

## Flagged: PRD ambiguities and conflicts

### Locked (decided before the build)

| Item | Decision |
|---|---|
| Default branch | Rename `master` → `main`; the PRD's `main` stands |
| Admin write scope | **Read-only** on others' applications; writes require ownership |
| Supabase connection | Single `DATABASE_URL` on the **session pooler**; the PRD's three-env-var rule stays literally true |
| Seed admin | `users.EnsureAdmin`, called at startup when `ADMIN_EMAIL`/`ADMIN_PASSWORD` are set, and by `make seed-admin` |
| `changed_by` FK | `on delete cascade`; **PRD amended** |
| `?q=` implementation | UNION of a ts_rank'd text branch and a fixed-rank-1.0 company branch; **PRD §Endpoints rewritten** |
| Dockerfile timing | Built in Phase 1 to enable an early Render deploy |
| `limit` over 100 | **Clamped** to 100, not rejected |
| `JWT_SECRET` length | **Minimum 32 bytes**, enforced at `NewIssuer` (RFC 7518 §3.2). Rotate the Render value before Phase 4 |
| UUID handling | `github.com/google/uuid` for `Principal.ID` and the `sub` claim. Not named in PRD §Stack, but it is a parse-and-validate step, not a substitute for anything listed — a malformed `sub` is rejected at `Verify` instead of reaching SQL |
| Oversized request body | **413**, not 400 — the body is well-formed, just over the 1 MB cap |
| PATCH absent vs. explicit null | A generic `Optional[T]{Set bool; Value *T}` rather than `*string`. `*string` cannot tell `{}` from `{"notes": null}` — both arrive as nil — and the PRD wants one to leave the column alone and the other to clear it |
| `?q=` company match | `escapeLike` escapes `\`, `%` and `_` before wrapping the term in `%...%`. **Deviates from the PRD's literal `'%' || $1 || '%'`**, under which a search for `100%` matches every company on file |
| Malformed uuid in a path | **404**, not 400. `/applications/garbage` is a URL that identifies nothing; 400 would be a second way to distinguish shapes of non-existent id, and 404 keeps every single-item route answering one thing |
| `middleware.RealIP` | **Omitted.** Spoofable via `X-Forwarded-For` and nothing keys off the client address |

### Internal conflicts in the PRD

**1. `?q=` search vs. the `EXPLAIN` requirement — RESOLVED, PRD amended.** The original spec OR'd the tsvector predicate with `companies.name ilike '%q%'` in one `where`. That sank company-only matches to `ts_rank = 0` (sorted last regardless of relevance) and made the whole predicate index-ineligible, contradicting the DoD's Bitmap Index Scan requirement.

*Now specified as* a `union all` of two branches — the tsvector predicate ranked by `ts_rank`, and the company `ilike` at a fixed rank of `1.0` — deduplicated by `max(rank)` per id and ordered `rank desc, applied_on desc, id`. Branch 1 stays a clean Bitmap Index Scan on `applications_search_idx` (integration test 12 asserts exactly that), and company matches now outrank incidental body-text mentions instead of sorting last. Folding company name into the tsvector was never an option: a generated column can't reference another table.

**2. `internal/httpx` is needed before it's scheduled.** Auth middleware (phase 3) writes `{"error": ...}` bodies, but httpx sits in phase 4. → Pulled forward to phase 3.

**3. Nothing can build the router for tests.** "Spin up handlers against a real Postgres" needs importable route wiring, but the PRD puts all of it in `cmd/api/main.go`, and `package main` can't be imported. → Added `internal/server/router.go`.

**4. `status_transitions.changed_by` has no `ON DELETE` — RESOLVED, PRD amended.** With `users` deletion cascading to `applications`, a user who authored transitions couldn't be deleted at all — an FK violation that breaks test teardown. → `on delete cascade`, now in the PRD schema.

**5. `updated_at` has a default but no updater.** No trigger is specified. → Set explicitly in every UPDATE (simpler to explain than a trigger).

### Ambiguous — proceeding with these defaults

| # | Question the PRD doesn't answer | Default |
|---|---|---|
| 1 | `applied_on` wire format | `"YYYY-MM-DD"` string in and out, not RFC3339 timestamps |
| 2 | PATCH: absent field vs. explicit null | `Optional[T]` — absent leaves the column alone, explicit `null` clears `location`/`notes`. See the locked decision above for why `*string` can't do it |
| 3 | PATCH with `status` in the body | **400**, not silent ignore. Transitions have their own endpoint for a reason |
| 4 | Non-owner on PATCH/DELETE/transition | 404, same as GET. Uniform, and never confirms existence |
| 5 | Ownership on `GET /{id}/history` | Same rule as `GET /{id}` |
| 6 | Does creating an application seed a history row? | No. History starts empty; the first entry is the first real transition, so `from_status` stays meaningful |
| 7 | `?status=` combined with `?q=` | AND |
| 8 | `limit` / `offset` bounds | `limit` default 20, **clamped** to 100 (over-max is silently capped, not a 400); `offset` default 0; negatives → 400 |
| 9 | List response shape | Bare JSON array. No total count is available without a second query, and pagination metadata is out of scope |
| 10 | Does `POST /auth/register` return a token? | No — 201 with `{id, email, role, created_at}`. Login is the next call, matching the README quickstart |
| 11 | Email / password validation | `net/mail.ParseAddress`; password 8–72 bytes (72 is bcrypt's hard limit); duplicate email → 409 |
| 12 | `GET /admin/users` fields and paging | `id, email, role, created_at`, never the hash; unpaginated |
| 13 | `/healthz` when the DB ping fails | **503** with `{"error": ...}`. A health check that always returns ok isn't one. Confirmed — also what Render's health check keys off |
| 14 | Company name uniqueness | `unique(name)` verbatim, names trimmed of whitespace. "Visa" and "visa" therefore create two rows — accepted limitation; a `lower(name)` unique index is the fix if it ever matters |
| 15 | `go.mod` version vs. the `golang:1.22` builder | **Settled in Phase 1:** `golang-migrate/v4` forces `go 1.25.11`, builder is `golang:1.26-bookworm`, no `toolchain` line, `GOTOOLCHAIN=local`. PRD §Docker amended |
| 16 | staticcheck vs. golangci-lint | staticcheck, version-pinned. Faster, and the PRD prefers it |

### Environment landmines (found on this machine)

| Finding | Impact |
|---|---|
| ~~`make` is broken~~ | **Fixed** — `xcode-select --switch` applied, GNU Make 3.81 responds |
| GNU Make **3.81** (2006) once fixed | Makefile must avoid `.ONESHELL`, `$(file ...)`, and other Make 4.x syntax |
| No host `psql` | Manual DB inspection goes through `docker compose exec postgres psql` |
| No `migrate` CLI | Migrations run from Go via embedded SQL — which is what the distroless image needs anyway |
| Go 1.27.1 local vs. `go 1.25.11` in `go.mod` | Handled: `GOTOOLCHAIN=local` is exported by the Makefile and set in the Dockerfile, so no toolchain is auto-downloaded and the builder fails loudly if it is too old |
| `go get` silently raises the `go` directive | Adding the Phase-3 deps bumped `go.mod` from `1.25.11` to `1.26.0` (via a transitive `x/crypto` that wanted it), and `go mod tidy` **never lowers it back**. Pinned `x/crypto v0.53.0`, reset the directive by hand, confirmed with `go list -m -f '{{.GoVersion}}' all` that nothing else demands more. Check `head -3 go.mod` after any `go get` — the Dockerfile builder tag is the thing that breaks |
| ~~Default branch is `master`~~ | **Fixed** — renamed to `main` |
