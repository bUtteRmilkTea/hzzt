# Asset Management App: Backend Implementation Plan (v2 — Go)

| Field | Value |
|---|---|
| Document | Backend plan, single source of truth for the API |
| Scope | Backend API only. The frontend plan is a separate document. |
| Clients | One responsive web app (mobile-first, also used on desktop). No native app. |
| Stack | **Go, chi, sqlc, pgx, goose, PostgreSQL** (decided — see D-01) |
| Dev environment | WSL2 (Ubuntu) + Docker, GitHub-backed repo (new Phase P0) |
| Status | Planning. P0 is Ready now. |
| Last updated | 2026-11-17 |

---

## 0. How to use this document

1. Each **phase** is sized for **one coding-agent session (Claude Sonnet, medium effort)**: one domain package, roughly 6 to 11 tasks, and roughly 10 to 20 files touched.
2. To run a phase, give the agent **Sections 1 to 6** (global context) plus **the target phase section**, using the brief template in **Appendix A**.
3. The agent implements **only that phase**. It must not change contracts from earlier phases unless the phase explicitly says so.
4. A phase is **Done** only when its acceptance criteria are verified with evidence: passing tests plus the agent's report (Appendix A, Deliverables). Code being generated does not make a phase Done.
5. Status legend: `Backlog`, `Ready`, `In Progress`, `Blocked`, `Review`, `Done`.
6. **P0 is different from the rest**: it sets up your machine and repository. Some of its steps are things you do by hand (installing WSL, Docker Desktop, creating the GitHub repo); others can be handed to a coding agent once WSL is up (writing `docker-compose.yml`, the `Makefile`, config files). Each P0 task says who does it.

---

## 1. Product summary

A personal finance and net-worth tracker. Users record cash wallets, income and expenses, transfers, investments (with live prices for crypto and US stocks), other assets and debts, and savings goals. Data lives inside **Spaces**, which can be shared with up to 5 members. Investments can also be **co-owned by several people with percentage shares**.

### 1.1 Confirmed requirements (from you)

| # | Requirement |
|---|---|
| R1 | Responsive web app; the backend serves a single web client used on mobile and desktop |
| R2 | Auth: email + password with JWT. **No SSO** (Google/Apple on the reference login screen are dropped) |
| R3 | Settings limited to: Profile, Space settings (portfolio), Goals, Category & Budget, App lock, Hide amounts, Keyboard type (mini calculator / standard numeric), Automatic currency conversion |
| R4 | Crypto auto pricing via **CoinMarketCap API** |
| R5 | US stock auto pricing via **Massive API** (formerly Polygon.io) |
| R6 | **Shared investments**: one investment co-owned by N people, each with their own percentage share |

### 1.2 Inferred from the screenshots (in scope unless you say otherwise)

| Feature | Evidence |
|---|---|
| Wallets (bank, e-wallet, cash) with card styling: solid, 2-color gradient with 8 directions, presets, templates | `bank_list`, `solid_edit_bank_card`, `gradation_edit_bank_card` |
| Wallet detail, "Add balance" (top-up), balance edit recorded as an adjustment, favorite, delete | `bank_detail`, `quick_edit_bank`, `bank_create` |
| Transaction history with search, day grouping, and filters (date presets, categories, wallets) | `transaction_page`, `filter_history`, `bank_history` |
| Transfers from a wallet or asset to a wallet or asset | `bank_transfer` |
| Expense and income categories with optional budgets and suggestions | `expense_list`, `ncome_list`, `expense_income_category` |
| Investments: Crypto, Indonesian stock, US stock, Mutual fund, US/Global ETF, each with auto or manual pricing | `*_create` screens |
| Asset page: Cash, Investments, Fixed assets, Money owed, Debt, Net-worth graph | `asset_page`, `portofolio` |
| Goals with an optional reserve taken from a wallet's available balance | `goal`, `add_goal` |
| Home dashboard: period overview, category breakdown, weekly analytics, calendar | `home` |
| Space settings: name/avatar, tracking mode, currency, invitation link/code (7-day expiry, 5 members max) | `settings` (Space Setting) |

### 1.3 Out of scope (v1)

SSO, AI receipt scanning, voice input, "Consult", "Insight", reminders, recurring transactions, tags, push notifications, notification reader, CSV import/export, bank statement import, NFC e-money, Pro plan and billing, language switching, and native mobile apps. Dark mode and the mini-calculator logic are frontend-only; the backend only stores the keyboard preference.

### 1.4 Assumptions

- **A1.** No existing codebase was provided, so this is a greenfield plan. If a codebase exists, run a Preparation review before P1.
- **A2.** The screenshots are a product and UI reference, not a pixel spec for the backend.
- **A3.** Preferences (hide amounts, keyboard, app lock) are **per user**. Currency settings are **per space** because they affect numbers that every member sees.
- **A4.** A space has one base (display) currency. Every money value is stored in its **native currency** and converted for totals.

---

## 2. Decisions

| ID | Decision | Status | Chosen | Blocks |
|---|---|---|---|---|
| D-01 | Backend stack | **Resolved** | **Go 1.23+, chi router, sqlc, pgx, goose migrations, PostgreSQL** | P1 |
| D-02 | Hosting | Open | Railway/Render/Fly + managed Postgres (Neon/Supabase/RDS); any of these run a Go binary or a small Docker image equally well | P19 |
| D-03 | FX rate provider | Recommended | **Frankfurter** (free, ECB daily, includes IDR) | P5 |
| D-04 | Auto-price coverage | Recommended | Massive covers US_STOCK + GLOBAL_ETF (US-listed); ID_STOCK and MUTUAL_FUND stay manual | P10, P12 |
| D-05 | Massive plan tier | Recommended | Build both EOD and snapshot strategies behind an env flag; start with EOD (free tier) | P12 |
| D-06 | Shared-investment participants | Recommended | **Both**: email invite for app users, name-only label for people without an account | P14 |
| D-07 | Net-worth attribution of shared investments | Recommended | **User's share only** | P14, P17 |
| D-08 | Space invitations | Recommended | Free in v1 (billing is out of scope) | P3 |
| D-09 | Spending below goal reserves | Recommended | **Allow and flag** `overReserved` | P16 |
| D-10 | Budget period start day | Recommended | Store with default day 1, not exposed in settings in v1 | P3, P8 |
| D-11 | App-lock PIN storage | Recommended | **Server-side argon2id hash** (syncs across devices) | P4 |
| D-12 | Email provider | Recommended | Resend, behind an interface | P2 |
| D-13 | Tracking mode semantics | Recommended | Support both `FULL_NET_WORTH` and `CASH_ONLY` | P3, P15, P17 |
| D-14 | Allow negative wallet balances | Recommended | **Allow** (overdrafts, corrections) | P7, P9 |
| D-15 | Investment accounts: overlap of wrapper shares and per-investment shares | Proposed (awaiting confirmation) | Investments **inside** an account take the account's shares; only ungrouped investments keep individual sharing | P18 |

Items marked "Recommended" carry forward from the original plan; flag any you'd like to revisit before their phase starts.

---

## 3. Tech stack (Go)

| Concern | Choice | Notes |
|---|---|---|
| Language | Go 1.23+, Go modules | |
| HTTP router | `chi` v5 | Lightweight, stdlib-compatible `net/http`, good middleware chain |
| Database | PostgreSQL 16 | `citext` for email, `NUMERIC` for money, `JSONB` for type-specific investment fields |
| DB driver | `pgx/v5` + `pgxpool` | Connection pooling, native Postgres types |
| Query layer | `sqlc` | Generates typed Go from hand-written `.sql` queries, checked against the real schema at generate time (catches breakage from schema drift immediately) |
| Migrations | `goose` | Plain SQL migration files, embedded into the binary with `go:embed`; `goose up`/`down`/`status` |
| Validation | `go-playground/validator/v10` for struct-tag checks (required, min/max, oneof) + hand-written Go functions for cross-field and business-rule checks (replaces Zod discriminated unions — see 5.1) |
| Auth | `golang-jwt/jwt/v5`; `alexedwards/argon2id` (thin wrapper over `golang.org/x/crypto/argon2`) for passwords and PINs |
| Money math | `shopspring/decimal`; Postgres `NUMERIC` columns map to `decimal.Decimal` via sqlc type overrides — **never `float64`** | |
| Scheduling | `robfig/cron/v3` for in-process schedules + `pg_advisory_lock` (via pgx) so only one instance runs a job | No Redis/queue in v1 (see TD-01) |
| Events | A small custom in-process pub/sub (a typed channel-based emitter) for things like "user registered" → "create default space" | No external broker needed at this scale |
| HTTP client | `net/http` wrapped with context timeouts, manual retry/backoff (or `hashicorp/go-retryablehttp`) | Used for CoinMarketCap, Massive, FX |
| Logging | `rs/zerolog` | JSON logs with request ID |
| API docs | `swaggo/swag` annotations on handlers → generated `openapi.yaml`/`docs.go`, served at `/api/docs` outside production | |
| Rate limiting | Custom middleware over `golang.org/x/time/rate`, keyed per IP or user, in-memory | Single instance in v1 |
| Email | Resend Go SDK behind an `EmailSender` interface; a logging/no-op driver for dev and tests | |
| Tests | Standard `testing` + `testify` (`assert`/`require`); real Postgres (Docker test DB); `httptest` for e2e; `jarcoal/httpmock` or `gock` for external API fixtures | |

---

## 4. Architecture

**Style:** modular monolith, one Go binary, one Postgres database.

```
cmd/
  api/
    main.go              # wiring: config, pool, router, cron, graceful shutdown
internal/
  config/                # env parsing + validation, fails fast on startup
  platform/
    httpx/                # error envelope, request ID, JSON helpers, pagination
    money/                # decimal helpers, currency minor units
    period/               # PeriodResolver
    apperror/             # AppError type + HTTP status mapping
  db/
    migrations/            # goose .sql files
    queries/                # sqlc input .sql, one file per domain
    sqlc/                    # generated Go (gitignored input, committed output or regenerated in CI — decide in P1)
  auth/                   # handlers, JWT, refresh rotation, password/PIN hashing
  users/                   # profile, preferences, app lock
  spaces/                  # spaces, members, invitations, space middleware
  currency/                # supported currencies, FX rates, conversion context
  categories/
  wallets/
  ledger/                  # the only package that changes wallet balances
  transactions/
  transfers/
  investments/
  marketdata/
    coinmarketcap/
    massive/
    jobs/                   # job runner, JobRun log
  sharedinvestments/
  manualassets/             # fixed assets, money owed, debt
  goals/
  dashboard/
  networth/
test/
  e2e/
  fixtures/
Makefile
sqlc.yaml
docker-compose.yml
```

**Request pipeline (chi middleware chain, in order):** panic recoverer → request ID → zerolog request logger → CORS (allowlist, credentials) → body size limit → rate limiter → `AuthMiddleware` (verifies JWT, loads the user into context; routes under `auth.Public(...)` opt out) → `SpaceMemberMiddleware` (routes under `/spaces/{spaceId}`, loads membership and role into context) → handler. Decoding and validation happen inside each handler using a shared `httpx.DecodeAndValidate[T](r)` helper, which keeps the chain generic and each handler's input type explicit.

**Cross-package rules**

- Only `ledger` mutates `wallets.balance`.
- Only `currency` converts money between currencies.
- Only `investments` (specifically its valuation code) computes investment values and P/L.
- Packages talk through exported functions/interfaces or the in-process event emitter, never by importing another domain package's sqlc queries directly for writes.

---

## 5. Global conventions (every phase must follow these)

### 5.1 API

- Base path `/api/v1`. JSON with camelCase fields. IDs are UUIDs (generated with `pgcrypto`'s `gen_random_uuid()` or Go's `google/uuid`).
- Space-scoped resources live under `/api/v1/spaces/{spaceId}/...`. Never accept `spaceId` from the request body.
- Lists use cursor pagination: `?limit=` (default 30, max 100) and `&cursor=`, returning `{ "items": [...], "nextCursor": "..." }`.
- Timestamps are ISO-8601 UTC. Date-only fields use `YYYY-MM-DD`.
- Update endpoints use `PATCH` with partial bodies, modeled in Go as pointer fields (`*string`, `*decimal.Decimal`) so "not sent" is distinguishable from "sent as empty/zero". `PUT` is used only for full replacement (e.g. a participant list).
- Request bodies with type-specific shapes (investment create, wallet card style, transfer source/destination) are decoded in two passes: first into a struct with a `Type`/`Mode` discriminator field plus `json.RawMessage` for the rest, then re-decoded into the concrete struct for that type once the discriminator is known. This is the Go equivalent of a Zod discriminated union — write it once in `platform/httpx` and reuse it.

### 5.2 Money and quantities

- DB: money is `NUMERIC(28,8)`; quantities (crypto units, shares) are `NUMERIC(38,18)`; FX rates are `NUMERIC(28,12)`.
- Go: every money or quantity value is `decimal.Decimal` (`shopspring/decimal`) — **never `float64`**, anywhere in the codebase. sqlc is configured (in `sqlc.yaml`, done in P1) to map `NUMERIC` columns directly to `decimal.Decimal`.
- API: money is a **string** (`"134.26"`) and always travels with a `currency` (ISO 4217). `decimal.Decimal` has a `MarshalJSON`/`UnmarshalJSON` pair configured to produce and accept strings, not JSON numbers. Quantities are strings too.
- Money amounts in transactions are always **positive**. The balance effect is determined by the transaction type (see P7).
- Rounding happens only in (a) share allocation (P14) and (b) presentation. Allocation rounds to the currency's minor units (IDR 0, USD 2) using the largest-remainder method.

### 5.3 Errors

```json
{ "error": { "code": "WALLET_NOT_FOUND", "message": "Wallet not found", "details": {} }, "requestId": "..." }
```

- `apperror.AppError{Code, HTTPStatus, Message, Details}` is the one error type every handler returns; a central `httpx.WriteError(w, r, err)` writes the envelope and logs it. Unrecognized errors become 500 `INTERNAL_ERROR` with no internal detail leaked.
- Status mapping: 400 validation (`VALIDATION_FAILED`, details are field errors), 401 `UNAUTHENTICATED`, 403 `FORBIDDEN` (role inside your own space), 404 not found (also used for resources in other spaces, to avoid leaking existence), 409 conflicts, 422 business-rule violations, 429 rate limit, 502 upstream provider failure.
- Error codes are `SCREAMING_SNAKE_CASE` and listed in Appendix B. Add new codes there as `apperror` constants in the owning package.

### 5.4 Auth

- `Authorization: Bearer <accessToken>` (JWT, 15 minutes), verified by `AuthMiddleware`.
- Refresh token in an httpOnly cookie `rt` with path `/api/v1/auth`, `Secure` in prod, `SameSite=Lax`. Deploy the API and web app on the **same site** (e.g. `app.example.com` and `api.example.com`) so Lax cookies work and no CSRF token is needed.

### 5.5 Multi-tenancy

Every space-scoped table has `space_id`. Every sqlc query filters by it explicitly — there is no automatic scoping, so this is a code-review checklist item in every phase (and the subject of the P19 isolation sweep). Handler functions take `spaceID uuid.UUID` from context, set by `SpaceMemberMiddleware`, and pass it down to every query call. Every phase adds integration tests proving that user B gets **404** on user A's resources.

### 5.6 Time

- `users.timezone` (IANA, default `UTC`; the frontend sends the browser timezone at registration), loaded with Go's `time.LoadLocation`.
- All period presets ("Today", "This Week", "This Period", and so on) are resolved in the requesting user's timezone by `period.Resolver` (built in P8).

### 5.7 Deletion

- Soft delete (`deleted_at`): users, spaces, wallets, categories, investments, manual assets, goals.
- Hard delete: transactions, which are always reversed through the ledger in the same DB transaction.
- Soft-deleted rows are excluded from lists and totals but kept for history (sqlc queries filter `WHERE deleted_at IS NULL` explicitly).

### 5.8 Concurrency

All balance and position changes happen inside a `pgx.Tx` using atomic `UPDATE ... SET balance = balance + $1`, or `SELECT ... FOR UPDATE` for read-modify-write (investment positions). A shared `db.WithTx(ctx, pool, fn)` helper in `platform` handles begin/commit/rollback so every phase uses the same pattern.

### 5.9 Testing (minimum for every endpoint)

Integration tests (build tag `e2e`, run with `go test -tags=e2e ./...`) must cover: happy path, validation failure, unauthenticated request, cross-space isolation, and the key business rules, using `httptest.NewServer` against the real router and a real test-database transaction per test (rolled back after). Pure logic (money, periods, valuation, allocation, FX) gets table-driven unit tests with `testify`.

### 5.10 Definition of Done (per phase)

1. All tasks implemented.
2. Goose migrations committed; `sqlc generate` run and the generated code committed (or regenerated in CI — confirm in P1).
3. `go vet ./...`, `golangci-lint run`, `go test ./...`, and `go test -tags=e2e ./...` all pass.
4. `swag init` run; new endpoints have annotations.
5. Appendix B is updated with new error codes.
6. The agent's report lists files changed, endpoints, migration file names, test results, and deviations from this plan.

---

## 6. Data model reference

Full field lists are defined in each phase. This is the map.

| Entity | Phase | Key relations / notes |
|---|---|---|
| User | P2 | email (citext, unique), password_hash, name, timezone, last_active_space_id |
| RefreshToken | P2 | user_id, family_id, token_hash, rotation chain |
| PasswordResetToken | P2 | user_id, token_hash, expires_at, used_at |
| Space | P3 | name, avatar, base_currency, auto_fx_conversion, tracking_mode, budget_period_start_day |
| SpaceMember | P3 | space_id + user_id, role OWNER/MEMBER |
| SpaceInvitation | P3 | code, expires_at (7 days) |
| UserPreference | P4 | hide_amounts, keyboard_type, app_lock fields |
| FxRate / ManualFxRate | P5 | daily provider rates; per-space manual rates |
| Category | P6 | type EXPENSE/INCOME, icon, color, budget_amount |
| Wallet | P7 | type, currency, cached balance, card_style (JSONB), is_favorite |
| Transaction | P7 | INCOME / EXPENSE / TRANSFER / ADJUSTMENT / OPENING_BALANCE; optional links to investment or manual asset |
| Investment | P10 | asset_type, price_mode, position fields, details (JSONB), is_shared |
| MarketInstrument / MarketPrice | P11 | provider + external_id; latest price |
| InvestmentParticipant / InvestmentShareChange | P14 | share_bps, status, attached_space_id |
| ManualAsset / ManualAssetValueChange | P15 | kind FIXED_ASSET/RECEIVABLE/LIABILITY |
| Goal / GoalAllocation | P16 | earmarks against wallets |
| NetWorthSnapshot | P17 | space_id + date, component totals |
| InvestmentAccount | P18 | kind SECURITIES/EXCHANGE; wrapper over investments; shareable like an investment |
| JobRun | P11 | name, status, timings, error |

---

## 7. Roadmap and phase tracker

| Phase | Title | Depends on | Milestone | Status |
|---|---|---|---|---|
| P0 | Environment & repository setup (WSL, Docker, Postgres, Go, GitHub) | — | M0 Dev environment | Ready |
| P1 | Project foundation | P0 | M1 Foundation & Auth | Done |
| P2 | Authentication | P1 | M1 | Done |
| P3 | Users, profile & spaces | P2 | M1 | Done |
| P4 | Preferences & app lock | P3 | M1 | Done |
| P5 | Currency & FX | P3 | M2 Money core | Done |
| P6 | Categories & budgets | P3 | M2 | Done |
| P7 | Wallets & ledger foundation | P5 | M2 | Done |
| P8 | Income/expense transactions & querying | P6, P7 | M2 | Done |
| P9 | Wallet-to-wallet transfers | P8 | M2 | Done |
| P10 | Investments core (manual pricing) | P5 | M3 Investments | Done |
| P11 | Crypto auto pricing (CoinMarketCap) | P10 | M3 | Done |
| P12 | US stock / ETF auto pricing (Massive) | P11 | M3 | Done |
| P13 | Investment buy/sell through wallets | P9, P10 | M3 | Done |
| P14 | Shared investments | P10, P13 | M3 | Done |
| P15 | Other assets & net-worth summary | P9, P14 | M4 Net worth & planning | Done |
| P16 | Goals | P7 | M4 | Done |
| P17 | Dashboard, analytics & net-worth history | P8, P15 | M4 | Done |
| P18 | Investment accounts (securities & exchanges) | P10, P13, P14 | M4 | Backlog |
| P19 | Hardening, docs & deployment | all | M5 Production | Backlog |

Critical path: P0 → P1 → P2 → P3 → P5 → P7 → P8 → P9 → P13 → P14 → P15 → P17 → P18 → P19. P4, P6, P10 to P12, and P16 can run in parallel branches once their dependencies are done.

---

## 8. Phases

### P0: Environment & repository setup (WSL, Docker, Postgres, Go, GitHub)

**Goal:** A working Linux dev environment on your Windows PC, a GitHub repo it's connected to, and Postgres running in Docker — nothing application-specific yet. This phase is a mix of things you do by hand and small config files a coding agent can write once WSL exists.
**Depends on:** nothing · **Size:** Small-Medium · **Who:** steps marked **(you)** are manual; steps marked **(agent)** can be handed to Claude Code once you're inside WSL.

| ID | Task | Who | Detail |
|---|---|---|---|
| P0-T01 | Install WSL2 + Ubuntu | **(you)** | PowerShell as admin: `wsl --install`, then restart. Confirm with `wsl -l -v` that the distro shows version 2. |
| P0-T02 | Install Docker | **(you)** | Either Docker Desktop (enable "Use the WSL 2 based engine" and WSL integration for your Ubuntu distro under Settings → Resources → WSL Integration), or Docker Engine installed directly inside Ubuntu if you'd rather skip Docker Desktop's GUI/licensing. Confirm with `docker run hello-world` from inside WSL. |
| P0-T03 | Install Go inside WSL | **(you)** or **(agent)** | Inside Ubuntu: download the official Go tarball (`go.dev/dl`), extract to `/usr/local`, add `export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin` to `~/.bashrc`. Confirm with `go version` (expect 1.23+). Do **not** rely on a Windows-side Go install — keep everything inside the Linux filesystem. |
| P0-T04 | Install CLI tools | **(agent)** | `go install github.com/pressly/goose/v3/cmd/goose@latest`, `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`, `go install github.com/swaggo/swag/cmd/swag@latest`, `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`. Confirm each with `--version`. |
| P0-T05 | VS Code setup | **(you)** | Install the **WSL** and **Go** VS Code extensions on the Windows side. Open the project from inside Ubuntu with `code .` so VS Code runs its server inside WSL, not on Windows. |
| P0-T06 | Create the GitHub repo | **(you)** | Create an empty repo on GitHub (no README/license yet, to avoid a merge-conflict on first push). Note the SSH or HTTPS URL. |
| P0-T07 | Set up SSH auth to GitHub from WSL | **(you)** | `ssh-keygen -t ed25519 -C "your-email"` inside WSL (a **separate key** from any Windows-side key), add the public key to GitHub → Settings → SSH keys, confirm with `ssh -T git@github.com`. |
| P0-T08 | Scaffold and push | **(agent)** | Inside `~/projects/asset-app` (Linux filesystem — **not** `/mnt/c/...`, which is much slower for file watching and builds): `git init`, `.gitignore` for Go (`/bin`, `*.env`, `.env`, generated sqlc output if you choose not to commit it), `go mod init github.com/<you>/asset-app`, a one-line `README.md`, commit, `git remote add origin <url>`, `git push -u origin main`. |
| P0-T09 | `docker-compose.yml` for Postgres | **(agent)** | Two services: `postgres` (dev, port 5432) and `postgres_test` (test, port 5433), both `postgres:16`, with named volumes so dev data survives restarts and the test volume can be wiped freely. `.env.example` with `DATABASE_URL` and `TEST_DATABASE_URL`. |
| P0-T10 | `Makefile` | **(agent)** | Targets: `up` (docker compose up -d), `down`, `migrate-up`, `migrate-down`, `migrate-status`, `sqlc-gen`, `run`, `test`, `test-e2e`, `lint`. This is what every later phase's agent session will actually run. |
| P0-T11 | Verify end-to-end | **(you)**, confirming the agent's work | `make up` → Postgres container healthy; `goose -dir db/migrations postgres "$DATABASE_URL" status` runs (even with zero migrations) without connection errors; `git log` shows the pushed commit on GitHub. |

**Acceptance criteria**
- `wsl -l -v` shows an Ubuntu distro on WSL **version 2**.
- `docker run hello-world` succeeds from inside that WSL distro.
- `go version` inside WSL reports 1.23 or newer.
- The repo exists on GitHub and `git push` from WSL succeeds over SSH.
- `make up` brings up two healthy Postgres containers on 5432 and 5433, confirmed with `docker compose ps`.
- The project lives under the Linux filesystem (`echo $PWD` does not start with `/mnt/c`).

**Why this matters for later phases:** P1 onward assumes `goose`, `sqlc`, and `make` targets exist and that Postgres is reachable at `DATABASE_URL`. Skipping or half-finishing P0 will make every later phase's "run the tests" step fail for environment reasons that look like code bugs.

---

### P1: Project foundation

**Goal:** A runnable, tested Go API skeleton with router, config, database pool, migrations, sqlc wiring, error envelope, and logging. No domain features yet.
**Depends on:** P0 · **Size:** Medium

**Out of scope:** any domain table, auth logic.

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P1-T01 | Module + layout | `go mod init`, create the package layout from Section 4, `golangci-lint` config (`.golangci.yml`: govet, staticcheck, errcheck, unused) | Critical | S |
| P1-T02 | Config | `internal/config`: parse env vars into a struct with manual validation (fail fast with a readable list of missing/invalid vars): `PORT, DATABASE_URL, JWT_ACCESS_SECRET, JWT_ACCESS_TTL, REFRESH_TTL_DAYS, CORS_ORIGINS, APP_URL, EMAIL_DRIVER, RESEND_API_KEY, CMC_API_KEY, MASSIVE_API_KEY, MASSIVE_MODE, FX_PROVIDER`. Provider keys are optional until their phase | Critical | S |
| P1-T03 | DB pool + migrations | `pgxpool.New` in `main.go`; embed `db/migrations` with `go:embed` and run `goose.Up` on boot (configurable, off in prod if migrations run as a separate deploy step — decide the exact approach in P19); enable `citext` and `pgcrypto` in migration `0001` | Critical | S |
| P1-T04 | sqlc wiring | `sqlc.yaml` pointing at `db/migrations` (schema) and `db/queries/*.sql`; configure `NUMERIC` → `decimal.Decimal`, `UUID` → `uuid.UUID`, `TIMESTAMPTZ` → `time.Time`; a trivial `SELECT 1` query to prove the pipeline, used by the health check | Critical | S |
| P1-T05 | Router + middleware chain | `chi.NewRouter()` with the Section 4 middleware chain; `cors.Handler` with an allowlist from config; 100KB body-size limit middleware | Critical | M |
| P1-T06 | Error envelope | `apperror.AppError`, `httpx.WriteError`, a panic-recovery middleware that converts panics into the 500 envelope instead of crashing the process | Critical | M |
| P1-T07 | Health endpoints | `GET /health` (liveness, no DB call), `GET /health/ready` (DB `SELECT 1` through the pool) | High | S |
| P1-T08 | Common utilities | `platform/money` (`decimal.Decimal` helpers: `MinorUnits(currency)`, string marshal/unmarshal), `platform/httpx` (cursor encode/decode as base64 of `{createdAt,id}`, `DecodeAndValidate[T]`, the discriminated-decode helper from 5.1) | Critical | M |
| P1-T09 | Test harness | `test/e2e` package with a `NewTestServer(t)` helper that spins up the router against the test database (`TEST_DATABASE_URL`) inside a transaction per test, rolled back in `t.Cleanup`; `go test -tags=e2e ./...` wired into the Makefile | Critical | M |
| P1-T10 | Swagger | `swag init` producing `docs/`; mount at `/api/docs` when `ENV != production`; bearer auth scheme annotated | Medium | S |
| P1-T11 | README | Setup, Makefile targets, env vars, conventions pointer to this document | Medium | S |

**Acceptance criteria**
- `make up && make migrate-up && make run` starts the API; `GET /api/v1/health` → 200 `{"status":"ok"}`; `/health/ready` → 200 when the DB is up and 503 when it is down.
- A missing `DATABASE_URL` makes startup fail with a clear message, not a panic with a bare stack trace.
- An unknown route returns the error envelope with status 404 and a `requestId`.
- Money unit tests pass, including `decimal.NewFromString("0.1").Add(decimal.NewFromString("0.2")).String() == "0.3"` and IDR's minor units being 0.
- All Makefile targets (`lint`, `test`, `test-e2e`) pass.

**Tests:** unit tests for `platform/money` and cursor encode/decode; e2e tests for health and the 404 envelope.

---

### P2: Authentication

**Goal:** Email/password registration and login with JWT access tokens, rotating refresh tokens, logout, password change, and password reset.
**Depends on:** P1 · **Size:** Medium-Large (the biggest early phase; keep strictly to scope)

**Data model**
- `users`: id, email (citext, unique, lowercased and trimmed), password_hash, name (1 to 60 chars), timezone (IANA, default `UTC`), last_active_space_id (nullable, FK added in P3), created_at, updated_at, deleted_at.
- `refresh_tokens`: id, user_id, family_id, token_hash (sha256), expires_at, revoked_at, replaced_by_id, user_agent, ip, created_at.
- `password_reset_tokens`: id, user_id, token_hash, expires_at, used_at, created_at.

**Endpoints**

| Method | Path | Notes |
|---|---|---|
| POST | /auth/register | `{email, password, name, timezone?}` → 201 `{accessToken, expiresIn, user}` + sets `rt` cookie |
| POST | /auth/login | `{email, password}` → same shape |
| POST | /auth/refresh | Reads `rt` cookie, rotates it, returns a new access token |
| POST | /auth/logout | Revokes the current refresh token family and clears the cookie |
| POST | /auth/logout-all | Revokes all of the user's refresh tokens |
| POST | /auth/change-password | Auth required; `{currentPassword, newPassword}`; revokes other sessions |
| POST | /auth/forgot-password | `{email}` → always 200 |
| POST | /auth/reset-password | `{token, newPassword}`; revokes all sessions |
| GET | /auth/me | Current user (minimal; extended in P3) |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P2-T01 | Migration + sqlc queries | Models above; goose migration `0002_auth.sql`; index `refresh_tokens(user_id)` and `(family_id)`; sqlc queries file `db/queries/auth.sql` | Critical | S |
| P2-T02 | Password hashing | `alexedwards/argon2id.CreateHash` / `.ComparePasswordAndHash`; policy 8 to 128 chars; reject if equal to email | Critical | S |
| P2-T03 | Token issuing | Access JWT `{sub, fam}` signed with `JWT_ACCESS_SECRET` (`golang-jwt/jwt/v5`), 15m. Refresh token is 32 random bytes (`crypto/rand`, base64url-encoded), stored hashed, TTL `REFRESH_TTL_DAYS` (30) | Critical | M |
| P2-T04 | Register and login handlers | Normalize email; duplicate email → 409 `EMAIL_ALREADY_REGISTERED`. Login uses the generic `INVALID_CREDENTIALS` error and runs a dummy argon2id comparison when the user is missing (timing safety). Emit a `user.registered` event through the P1 in-process emitter | Critical | M |
| P2-T05 | Refresh rotation and reuse detection | Each refresh marks the old token revoked with `replaced_by_id`. Presenting an already-revoked token revokes the **whole family** → 401 `REFRESH_TOKEN_REUSED` | Critical | M |
| P2-T06 | Cookie handling | `rt` cookie: httpOnly, Secure (prod), SameSite=Lax, path `/api/v1/auth`, MaxAge = TTL. Cleared on logout via `net/http.SetCookie` with `MaxAge: -1` | Critical | S |
| P2-T07 | AuthMiddleware | Verifies the bearer token, loads the user (rejects soft-deleted), puts it in request context; a small `auth.Public()` route-tagging convention lets chi routes opt out | Critical | S |
| P2-T08 | Password reset flow | Token stored hashed, 30-minute expiry, single use; link `${APP_URL}/reset-password?token=...`. `EmailSender` interface with a logging driver (dev/test) and a Resend driver | High | M |
| P2-T09 | Rate limits | login 5/min per IP+email; register 5/min per IP; forgot-password 3/15min per IP, via the P1 rate-limit middleware | High | S |
| P2-T10 | Swagger + error codes | `swag` annotations; add codes to Appendix B | Medium | S |

**Acceptance criteria**
- Register → login → call `/auth/me` with the access token → refresh → old refresh token rejected with family revoked → logout clears the cookie.
- Passwords are never returned or logged; only hashes are stored for refresh/reset tokens.
- `forgot-password` responds identically for known and unknown emails.
- After `reset-password`, all prior refresh tokens fail.
- An expired access token → 401 `UNAUTHENTICATED`.

**Tests:** e2e tests for every endpoint, including reuse detection and reset-token expiry and reuse; unit tests for token issuing/verification.

---

### P3: Users, profile & spaces

**Goal:** Profile management, spaces (default personal space at signup), membership, roles, invitations, and the space middleware used by every later phase.
**Depends on:** P2 · **Size:** Medium-Large

**Data model**
- `spaces`: id, name (1 to 40), avatar_emoji (nullable), avatar_color (hex), base_currency (char 3, default `USD`), auto_fx_conversion (bool, default true), tracking_mode (`FULL_NET_WORTH` / `CASH_ONLY`, default FULL), budget_period_start_day (1 to 31, default 1), created_by_id, created_at, updated_at, deleted_at.
- `space_members`: space_id, user_id, role (`OWNER` / `MEMBER`), joined_at; unique (space_id, user_id).
- `space_invitations`: id, space_id, code (8 chars, uppercase alphanumeric excluding ambiguous characters, unique), created_by_id, expires_at (+7 days), revoked_at, created_at.

**Endpoints**

| Method | Path | Role | Notes |
|---|---|---|---|
| GET | /me | any | user + last_active_space_id |
| PATCH | /me | any | name, timezone |
| DELETE | /me | any | `{password}`; blocked with 422 `OWNERSHIP_TRANSFER_REQUIRED` if the user owns a space that has other members; otherwise soft-deletes the user and their sole-member spaces and revokes tokens |
| GET | /spaces | any | my spaces with role and member count |
| POST | /spaces | any | creates a space, caller becomes OWNER, emits `space.created` |
| GET | /spaces/{spaceId} | member | details |
| PATCH | /spaces/{spaceId} | OWNER | name, avatar, base_currency, auto_fx_conversion, tracking_mode |
| DELETE | /spaces/{spaceId} | OWNER | soft delete; cannot delete your last remaining space (`LAST_SPACE`) |
| POST | /spaces/{spaceId}/activate | member | sets user.last_active_space_id |
| GET | /spaces/{spaceId}/members | member | name, email, role |
| DELETE | /spaces/{spaceId}/members/{userId} | OWNER, or self (leave) | the owner cannot leave (`OWNER_CANNOT_LEAVE`) |
| POST | /spaces/{spaceId}/invitations | OWNER | revokes any active code and creates a new one; returns code + link |
| GET | /spaces/{spaceId}/invitations/active | OWNER | current code or null |
| DELETE | /spaces/{spaceId}/invitations/active | OWNER | revoke |
| POST | /invitations/{code}/accept | any | joins as MEMBER |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P3-T01 | Migration + sqlc queries | Models above; FK `users.last_active_space_id` added here | Critical | S |
| P3-T02 | Default space on signup | Subscriber on `user.registered`: create "{firstName}'s Space" + OWNER membership + set active; emit `space.created` | Critical | S |
| P3-T03 | SpaceMemberMiddleware + role check | Resolves `{spaceId}`, checks membership (404 if not a member or space deleted), puts space + role in context; a small `spaces.RequireRole("OWNER")` wrapper → 403 `FORBIDDEN` for members | Critical | M |
| P3-T04 | Profile endpoints | `/me` GET/PATCH/DELETE; validate timezone by attempting `time.LoadLocation` | High | S |
| P3-T05 | Space CRUD + activate | As in the table. `base_currency` is validated against a `SupportedCurrencies` constant defined here and reused in P5 | Critical | M |
| P3-T06 | Members | List, remove, leave | High | S |
| P3-T07 | Invitations | Generate, revoke, accept. Errors: `INVITATION_INVALID`, `INVITATION_EXPIRED`, `ALREADY_A_MEMBER`, `SPACE_MEMBER_LIMIT` (max 5 including owner) | High | M |
| P3-T08 | Isolation test helper | Reusable e2e helper `expectCrossSpace404(t, route)` used by all later phases | Critical | S |

**Acceptance criteria**
- A new registration has exactly one space where the user is OWNER, and it is the active space.
- A non-member gets 404 on every `/spaces/{spaceId}/*` route; a MEMBER gets 403 on owner-only routes.
- A 6th member cannot join. Expired or revoked codes fail with the correct codes.
- Changing `base_currency` does not rewrite any stored amounts (conversion happens at read time, P5).

**Tests:** e2e tests for all endpoints, role matrix, invitation lifecycle, and account deletion rules.

---

### P4: Preferences & app lock

**Goal:** Store per-user UI preferences and provide a PIN app lock that syncs across devices.
**Depends on:** P3 · **Size:** Small-Medium

**Data model:** `user_preferences`: user_id (PK), hide_amounts (bool, default false), keyboard_type (`MINI_CALCULATOR` / `STANDARD_NUMERIC`, default MINI_CALCULATOR), app_lock_enabled (bool), app_lock_pin_hash (nullable), app_lock_failed_attempts (int), app_lock_locked_until (nullable), updated_at.

**Important:** the app lock is a **client-side UX gate**, not a security boundary. API access is still governed by the JWT. Document this in Swagger.

| Method | Path | Notes |
|---|---|---|
| GET | /me/preferences | row is created with defaults on `user.registered`; never returns the PIN hash |
| PATCH | /me/preferences | hide_amounts, keyboard_type |
| POST | /me/app-lock | enable: `{pin, password}`; PIN is 4 to 6 digits |
| POST | /me/app-lock/verify | `{pin}` → `{valid}`; 5 consecutive failures → locked for 5 minutes (`APP_LOCK_TEMPORARILY_LOCKED`, includes `retryAfter`) |
| PATCH | /me/app-lock/pin | `{currentPin, newPin}` |
| DELETE | /me/app-lock | `{password}` disables the lock |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P4-T01 | Migration + sqlc queries | Model above; row created by the same `user.registered` subscriber as P3-T02 | Critical | S |
| P4-T02 | Preferences GET/PATCH | `oneof` validator tags on the enum fields | High | S |
| P4-T03 | Enable/disable app lock | argon2id PIN hash; password re-check through the P2 password package | High | S |
| P4-T04 | Verify with lockout | Atomic counter update inside a transaction; reset on success | High | S |
| P4-T05 | Change PIN | Requires current PIN; counts toward the lockout | Medium | S |
| P4-T06 | Rate limit | verify 10/min per user | Medium | S |

**Acceptance criteria:** defaults are returned for new users; the 6th wrong PIN within the lock window returns 429/422 with `retryAfter`; the PIN hash never appears in any response; disabling requires the account password.

---

### P5: Currency & FX

**Goal:** Provide supported currencies, daily FX rates, manual rates, and one conversion context that every aggregation uses. This implements "switch currency automatically".
**Depends on:** P3 · **Size:** Medium

**Semantics**
- Every money value keeps its native currency.
- Totals are converted to `space.base_currency`.
- With `space.auto_fx_conversion = true`, live provider rates are used.
- With it `false`, the space's manual rates (`manual_fx_rates`) are used. A per-investment manual rate (the "USD/IDR rate" field, P10) overrides both for that investment only when auto mode is off.
- A missing rate never crashes a total. The value is excluded and reported in `warnings[]`.

**Data model**
- `fx_rates`: base (char 3, always `USD`), quote, rate, as_of (date), source, fetched_at; unique (base, quote, as_of).
- `manual_fx_rates`: space_id, base, quote, rate, updated_by_id, updated_at; unique (space_id, base, quote).

| Method | Path | Notes |
|---|---|---|
| GET | /currencies | supported list: code, name, symbol, minorUnits |
| GET | /fx/latest?base=IDR | latest rates re-based to `base` |
| GET | /spaces/{spaceId}/fx/manual-rates | member |
| PUT | /spaces/{spaceId}/fx/manual-rates/{base}/{quote} | OWNER; `{rate}` |
| DELETE | /spaces/{spaceId}/fx/manual-rates/{base}/{quote} | OWNER |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P5-T01 | Migration + sqlc queries | Models above | Critical | S |
| P5-T02 | Currency catalog | `SupportedCurrencies` Go map/slice (USD, IDR, EUR, GBP, SGD, MYR, JPY, AUD, CNY, KRW, THB, PHP, INR, HKD, CAD, CHF), restricted to codes the chosen provider returns | Critical | S |
| P5-T03 | FX provider adapter | `FxProvider` interface; `FrankfurterProvider` fetching latest rates with base USD (confirm the current base URL in the Frankfurter docs at implementation time) using a `net/http` client wrapper (10s timeout, 2 retries) | Critical | M |
| P5-T04 | FX refresh job | `robfig/cron` schedule every 6 hours plus on boot if no rates exist; `pg_advisory_lock` so only one instance runs it; upsert by as_of | High | S |
| P5-T05 | Convert function | `fx.Convert(amount decimal.Decimal, from, to string, ctx ConversionContext) (decimal.Decimal, error)`; identity for the same currency; cross rate via USD; uses the latest rate with `as_of ≤ at`; returns `ErrRateUnavailable` | Critical | M |
| P5-T06 | ConversionContext + aggregator | `fx.BuildContext(ctx, spaceID) (ConversionContext, error)` → `{baseCurrency, mode, manualRates, rateTable}` loaded once per request. `fx.Aggregate(items []MoneyItem) AggregateResult{Total, Currency, Breakdown, Warnings}` | Critical | M |
| P5-T07 | Endpoints | As in the table | High | S |

**Acceptance criteria:** with USD→IDR at 16,000, `Convert(10 USD → IDR)` = `160000`; IDR→EUR is computed through USD; manual mode uses the manual rate even when a live rate exists; a missing rate produces a warning, not an error; the job is idempotent within the same day.

**Tests:** unit tests for conversion math and context modes; provider adapter tested with a mocked HTTP fixture (`httpmock`); e2e tests for the endpoints.

---

### P6: Categories & budgets

**Goal:** Expense and income categories per space, default seeds, suggestions, and optional monthly budgets on expense categories. Usage stats come in P8.
**Depends on:** P3 · **Size:** Small-Medium

**Data model:** `categories`: id, space_id, type (`EXPENSE` / `INCOME`), name (1 to 30), icon (emoji), color (hex), budget_amount (nullable, expense only, in `space.base_currency`), sort_order, created_by_id, created_at, updated_at, deleted_at. Partial unique index on (space_id, type, lower(name)) where deleted_at is null.

| Method | Path | Notes |
|---|---|---|
| GET | /spaces/{spaceId}/categories?type= | ordered by sort_order |
| POST | /spaces/{spaceId}/categories | `{type, name, icon, color, budgetAmount?}` |
| PATCH | /spaces/{spaceId}/categories/{id} | type is immutable once used by transactions (`CATEGORY_TYPE_LOCKED`) |
| DELETE | /spaces/{spaceId}/categories/{id} | soft delete; historical transactions keep the link |
| PUT | /spaces/{spaceId}/categories/order | `{type, ids[]}` |
| GET | /categories/suggestions?type= | static list |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P6-T01 | Migration + sqlc queries | Model above; the partial unique index, which Postgres supports natively | Critical | S |
| P6-T02 | Default seeds | Subscriber on `space.created`. Expense: Transport 🚗, Shopping 🛍️, Subscriptions ✨, Health 💊, Gifts 🎁, Snacks 🍿, Family 👨‍👩‍👧. Income: Salary 💰, Freelance 💼, Investment 📈, Dividend 📈 | High | S |
| P6-T03 | CRUD + reorder | Duplicate name → 409 `CATEGORY_NAME_TAKEN`; `budget_amount` on INCOME → 422 `BUDGET_NOT_ALLOWED` | Critical | M |
| P6-T04 | Suggestions | Income: Salary, Bonus, Freelance, Investment, Gift, Refund. Expense: Food, Coffee, Transport, Groceries, Bills, Entertainment | Low | S |

**Acceptance criteria:** a new space has 7 expense and 4 income categories; names are unique per type and case-insensitive among non-deleted categories; a deleted name can be reused.

---

### P7: Wallets & ledger foundation

**Goal:** Wallets with card styling, cached balances, and the `transactions` table with the `ledger` package that owns every balance change. This phase introduces OPENING_BALANCE and ADJUSTMENT transactions.
**Depends on:** P5 · **Size:** Medium-Large

**Data model**
- `wallets`: id, space_id, type (`BANK` / `E_WALLET` / `CASH` / `OTHER`), name (1 to 30), icon (emoji), currency (char 3, default space base; immutable once transactions exist → `WALLET_CURRENCY_LOCKED`), balance (`NUMERIC`, cached), is_favorite, sort_order, card_style (JSONB), created_by_id, created_at, updated_at, deleted_at.
- `card_style`, decoded in Go via the discriminated-decode helper (5.1) into one of:
  - `{Mode: "SOLID", Color: "#4480EE"}`
  - `{Mode: "GRADIENT", Colors: ["#4480EE", "#0E76B7"], Direction: "N"|"NE"|"E"|"SE"|"S"|"SW"|"W"|"NW", PresetID: *string}`
  - `{Mode: "TEMPLATE", TemplateID: string}`
- `transactions` (shared by all later phases): id, space_id, type (`INCOME` / `EXPENSE` / `TRANSFER` / `ADJUSTMENT` / `OPENING_BALANCE`), direction (`IN` / `OUT`, only for ADJUSTMENT and OPENING_BALANCE), title (nullable), note (nullable), amount (> 0), currency, wallet_id (nullable), category_id (nullable), occurred_at, created_by_id, created_at, updated_at.
  - Transfer fields (used from P9): to_wallet_id, to_amount, to_currency, fx_rate.
  - Investment fields (used from P13): investment_id, investment_action (`BUY` / `SELL`), quantity, unit_price, fee, realized_pl, position_before (JSONB), position_after (JSONB).
  - Manual-asset field (used from P15): manual_asset_id.
  - Indexes: (space_id, occurred_at desc, id), (wallet_id, occurred_at), (to_wallet_id), (category_id), (investment_id).

**Balance effects (`ledger` is the only writer)**

| Type | Effect |
|---|---|
| INCOME | wallet += amount |
| EXPENSE | wallet −= amount |
| ADJUSTMENT / OPENING_BALANCE | wallet ± amount according to direction |
| TRANSFER | from −= amount; to += to_amount (P9) |

| Method | Path | Notes |
|---|---|---|
| GET | /spaces/{spaceId}/wallets | favorites first, then sort_order; each wallet includes balance; response includes `total` (converted, via the P5 aggregator) |
| POST | /spaces/{spaceId}/wallets | `{type, name, icon, currency?, initialBalance?, cardStyle?}`; a non-zero initialBalance creates an OPENING_BALANCE transaction titled "Opening balance" |
| GET | /spaces/{spaceId}/wallets/{id} | detail |
| PATCH | /spaces/{spaceId}/wallets/{id} | name, type, icon, card_style, is_favorite |
| PUT | /spaces/{spaceId}/wallets/{id}/balance | `{newBalance, note?}` → ADJUSTMENT for the delta; zero delta is a no-op |
| DELETE | /spaces/{spaceId}/wallets/{id} | soft delete; non-zero balance requires `?force=true` (`WALLET_HAS_BALANCE`) |
| PUT | /spaces/{spaceId}/wallets/order | `{ids[]}` |
| GET | /wallet-card-presets | static: e.g. Wallet color, Midnight, Ocean, Aurora, Sunset (colors + direction) |
| GET | /wallet-card-templates | static list of template IDs and names (images are frontend assets) |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P7-T01 | Migration + sqlc queries | Wallet + Transaction (all columns listed, even those used later, to avoid churn) | Critical | M |
| P7-T02 | Ledger package | `ledger.Apply(ctx, tx, txn)`, `ledger.Reverse(ctx, tx, txn)` taking an open `pgx.Tx`, using atomic `UPDATE ... balance = balance + $1`; `ledger.RecomputeBalance(ctx, walletID)` for maintenance and tests | Critical | M |
| P7-T03 | Card style validation | Discriminated decode + hex regex; template_id/preset_id must exist in the static catalogs | High | S |
| P7-T04 | Wallet CRUD + reorder + favorite | As in the table | Critical | M |
| P7-T05 | Opening balance on create | Same `pgx.Tx` as the wallet insert | Critical | S |
| P7-T06 | Balance set → adjustment | Computes the delta under `SELECT ... FOR UPDATE`; ADJUSTMENT with direction; title "Balance adjustment" | Critical | S |
| P7-T07 | Converted wallet total | Uses the P5 aggregator; returns warnings | High | S |
| P7-T08 | Invariant test helper | `expectLedgerConsistent(t, walletID)` asserts cached balance = recomputed balance; used in every later money phase | Critical | S |

**Acceptance criteria:** creating a wallet with initialBalance 134.26 gives balance 134.26 and one OPENING_BALANCE transaction; setting the balance to 100 creates an ADJUSTMENT OUT of 34.26; deleted wallets are excluded from totals; the invariant holds after every test.

---

### P8: Income/expense transactions & querying

**Goal:** Create, edit, and delete income and expense transactions; wallet top-up; filtered and searchable history; day summaries; period resolution; category usage stats.
**Depends on:** P6, P7 · **Size:** Medium-Large

**Period presets** (from the filter screen): `TODAY`, `THIS_WEEK` (Monday start), `THIS_PERIOD` (budget period from `space.budget_period_start_day`), `LAST_7_DAYS`, `LAST_30_DAYS`, `LAST_60_DAYS`, `LAST_90_DAYS`, `THIS_YEAR`, `ALL_TIME`, or a custom `from`/`to`.

| Method | Path | Notes |
|---|---|---|
| POST | /spaces/{spaceId}/transactions | `{type: INCOME\|EXPENSE, walletId, amount, categoryId?, title?, note?, occurredAt?}`; currency = wallet currency; the category type must match the transaction type (`CATEGORY_TYPE_MISMATCH`) |
| GET | /spaces/{spaceId}/transactions/{id} | |
| PATCH | /spaces/{spaceId}/transactions/{id} | INCOME/EXPENSE: all fields (reverse old effect + apply new in one `pgx.Tx`). ADJUSTMENT/OPENING_BALANCE: only note. TRANSFER/BUY/SELL: their own endpoints (`USE_SPECIFIC_ENDPOINT`) |
| DELETE | /spaces/{spaceId}/transactions/{id} | reverse + delete (BUY/SELL rules in P13) |
| GET | /spaces/{spaceId}/transactions | filters: `period`, `from`, `to`, `types[]`, `categoryIds[]`, `walletIds[]` (matches wallet_id or to_wallet_id), `q` (ILIKE on title, note, category name, wallet name); sorted occurred_at desc, id desc; cursor pagination |
| GET | /spaces/{spaceId}/transactions/daily-summary | same filters → `[{date, income, expense, transfer, count}]` in base currency, grouped by user-local date |
| POST | /spaces/{spaceId}/wallets/{id}/top-up | `{amount, note?}` → INCOME, no category, title "Balance top up" |
| GET | /spaces/{spaceId}/categories?includeStats=true&period= | extends P6: expense → `spent`, `budgetRemaining`, `budgetUsedPct`; income → `received` |

Since the filter list and query are optional in any combination, the sqlc query for the list endpoint uses the "nullable parameter" pattern — e.g. `WHERE ($1::uuid[] IS NULL OR category_id = ANY($1::uuid[]))` — rather than building SQL strings by hand.

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P8-T01 | PeriodResolver | `period.Resolve(preset, opts{timezone, budgetStartDay, now}) (Range, error)`. Start day 31 in February clamps to the last day of the month. Pure function, built with Go's `time` package | Critical | M |
| P8-T02 | Create/edit/delete | Through `ledger` inside one `pgx.Tx`; validate that the wallet and category belong to the space and are not deleted | Critical | M |
| P8-T03 | Top-up endpoint | Thin wrapper over create | High | S |
| P8-T04 | List with filters + search | sqlc query with the nullable-parameter pattern above; cursor on (occurred_at, id); joins to pull wallet/category name and icon | Critical | M |
| P8-T05 | Daily summary | SQL `GROUP BY (occurred_at AT TIME ZONE $tz)::date, currency`, then convert with the P5 aggregator | High | M |
| P8-T06 | Category stats | Aggregate per category and currency for the period → convert → merge into the category list | High | M |

**Acceptance criteria:** editing an expense from wallet A (50) to wallet B (70) restores A by 50 and reduces B by 70; filters combine with AND across types and OR within a type; `TODAY` for a user in `Asia/Jakarta` at 23:30 local includes only that local day; the ledger invariant holds; daily-summary totals equal the sum of the listed items.

**Tests:** PeriodResolver table-driven unit tests (month edges, leap year, start days 1/15/31, timezones); e2e tests for CRUD, filters, search, summary, and isolation.

---

### P9: Wallet-to-wallet transfers

**Goal:** Move money between wallets, including cross-currency transfers. The request contract is designed so P13 can add investments as source or destination.
**Depends on:** P8 · **Size:** Small-Medium

**Request contract**

```json
{
  "from": { "type": "WALLET", "walletId": "..." },
  "to":   { "type": "WALLET", "walletId": "..." },
  "amount": "100.00",
  "toAmount": "1600000",
  "occurredAt": "2026-11-17T06:40:00Z",
  "note": "optional"
}
```

`toAmount` is required when currencies differ (otherwise it must equal `amount` or be omitted). The stored `fx_rate` = to_amount / amount. In P9, `type: "INVESTMENT"` returns 422 `NOT_SUPPORTED_YET`; P13 enables it. `from`/`to` are decoded with the discriminated-decode helper from 5.1.

| Method | Path |
|---|---|
| POST | /spaces/{spaceId}/transfers |
| PATCH | /spaces/{spaceId}/transfers/{id} |
| DELETE | /spaces/{spaceId}/transfers/{id} |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P9-T01 | Contract + validation | Discriminated-decode for `from`/`to`; `TRANSFER_SAME_WALLET`; `TRANSFER_AMOUNT_REQUIRED` for cross-currency | Critical | S |
| P9-T02 | Create | One TRANSFER transaction; `ledger` applies both sides atomically inside one `pgx.Tx` | Critical | S |
| P9-T03 | Edit/delete | Reverse + reapply | High | S |
| P9-T04 | Totals semantics | Transfers are excluded from income/expense totals and appear in the `transfer` column of the daily summary (update the P8 aggregation query if needed) | High | S |

**Acceptance criteria:** a transfer of 100 USD to an IDR wallet with toAmount 1,600,000 updates both balances; a transfer filtered by either wallet appears in that wallet's history; the invariant holds for both wallets; negative balances are allowed (D-14).

---

### P10: Investments core (manual pricing)

**Goal:** CRUD for the five investment types with type-specific fields, plus the valuation engine. Auto-price modes are accepted but fall back to the manual price until P11/P12.
**Depends on:** P5 · **Size:** Medium-Large

**Data model:** `investments`: id, space_id, asset_type (`CRYPTO` / `ID_STOCK` / `US_STOCK` / `MUTUAL_FUND` / `GLOBAL_ETF`), price_mode (`AUTO` / `MANUAL`), name, symbol (ticker or coin, nullable for mutual funds), icon, quote_currency, quantity, avg_buy_price, manual_price (the "Current price" or "Current NAV" field), fees, manual_current_value (nullable), purchase_date (nullable), estimated_annual_change_pct (−100 to 1000, default 5.5), manual_fx_rate (nullable; the "USD/IDR rate" field), details (JSONB), instrument_id (nullable, from P11), is_shared (default false), created_by_id, created_at, updated_at, deleted_at.

**Type rules and `details` schema** (each asset type decodes `details` into its own Go struct via the discriminated-decode helper, keyed on `asset_type`)

| asset_type | quote_currency | Auto price | details | Quantity rule |
|---|---|---|---|---|
| CRYPTO | USD | yes (P11) | exchange_or_wallet?, network? (staked_quantity was removed in feedback round 3) | client sends quantity |
| ID_STOCK | IDR | no (D-04) | broker?, lots (integer ≥ 0) | **quantity = lots × 100**, server-derived |
| US_STOCK | USD | yes (P12) | exchange (e.g. NASDAQ, NYSE), platform? | shares |
| MUTUAL_FUND | client-chosen (default space base) | no | fund_manager?, fund_type (`MONEY_MARKET` / `FIXED_INCOME` / `EQUITY` / `BALANCED` / `OTHER`), nav_date? | units; avg_buy_price = purchase NAV; manual_price = current NAV |
| GLOBAL_ETF | USD | yes for US-listed (P12) | exchange, platform?, expense_ratio_pct?, asset_class? | shares |

**Valuation (`investments.Valuation`, a pure function + a context loader)**

1. `price` = market price if `price_mode = AUTO` and a market price exists, otherwise `manual_price`. `priceSource` = `MARKET` or `MANUAL`.
2. `marketValue` = `manual_current_value` if set (`priceSource = MANUAL_VALUE`), otherwise `quantity × price`.
3. `costBasis` = `quantity × avg_buy_price + fees`.
4. `unrealizedPl` = `marketValue − costBasis`; `unrealizedPlPct` = `unrealizedPl / costBasis` (null if costBasis is 0).
5. Convert to base currency. If the space is in manual FX mode and `manual_fx_rate` is set, use it for this investment; otherwise use the P5 ConversionContext.
6. `ownershipFactor` hook (returns 1 now; P14 changes it).
7. Output includes native and base values, `priceSource`, `priceAsOf`, and `isStale`.

| Method | Path | Notes |
|---|---|---|
| GET | /investment-types | metadata for dynamic forms: fields, defaults, supportsAutoPrice, quoteCurrency |
| GET | /spaces/{spaceId}/investments?assetType= | items with valuation + `summary {totalValue, totalCost, unrealizedPl, unrealizedPlPct}` in base currency |
| POST | /spaces/{spaceId}/investments | validated by asset_type |
| GET | /spaces/{spaceId}/investments/{id} | detail + valuation |
| PATCH | /spaces/{spaceId}/investments/{id} | asset_type is immutable |
| DELETE | /spaces/{spaceId}/investments/{id} | soft delete |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P10-T01 | Migration + sqlc queries | Model above; index (space_id, asset_type) | Critical | S |
| P10-T02 | Per-type structs + validation | One Go struct per asset_type with `validate` tags, selected via the discriminated-decode helper; AUTO on a type without auto support → 422 `AUTO_PRICE_NOT_SUPPORTED` | Critical | M |
| P10-T03 | Valuation function | Pure function + context loader; table-driven unit tests with fixtures for each type | Critical | M |
| P10-T04 | CRUD endpoints | ID_STOCK derives quantity from lots; CRYPTO validates staked ≤ quantity | Critical | M |
| P10-T05 | Portfolio summary | Aggregate converted values; warnings passed through | High | S |
| P10-T06 | Investment types endpoint | Static metadata mirroring the per-type structs | Medium | S |

**Acceptance criteria:** BBCA with 2 lots → quantity 200; a manual_current_value overrides quantity × price; P/L % matches hand-calculated fixtures; a USD investment in an IDR space with auto FX off and manual_fx_rate 16,000 converts using 16,000.

---

### P11: Crypto auto pricing (CoinMarketCap)

**Goal:** Coin search plus scheduled live prices for AUTO crypto investments, with shared market-data infrastructure (instruments, prices, job log).
**Depends on:** P10 · **Size:** Medium

**Data model**
- `market_instruments`: id, provider (`CMC` / `MASSIVE`), external_id (CMC numeric id or ticker), symbol, name, kind (`CRYPTO` / `STOCK` / `ETF`), currency, rank (nullable), active, updated_at; unique (provider, external_id).
- `market_prices`: instrument_id (PK), price, currency, change_24h_pct (nullable), as_of, fetched_at.
- `job_runs`: id, name, started_at, finished_at, status (`SUCCESS` / `FAILED` / `SKIPPED`), items_processed, error.

**CoinMarketCap usage** (verify against the current CMC docs and your plan's credit limits before implementing)
- Coin list: `GET /v1/cryptocurrency/map` → synced daily into `market_instruments` (top ~5,000 by rank) so search costs no credits.
- Quotes: `GET /v2/cryptocurrency/quotes/latest?id=1,1027,...&convert=USD`, batched up to 100 ids per call.
- Auth: header `X-CMC_PRO_API_KEY`, server-side only.
- Budget: every 15 minutes ≈ 96 calls/day ≈ 2,900/month for up to 100 coins (the default; it was 10 minutes, ≈ 4,300/month). Make this configurable with `CMC_REFRESH_MINUTES`.

| Method | Path | Notes |
|---|---|---|
| GET | /market/crypto/search?q= | local search by symbol or name, ranked; returns instrumentId, symbol, name, rank |
| POST | /spaces/{spaceId}/investments/{id}/refresh-price | on-demand refresh; 1/min per investment |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P11-T01 | Migration + sqlc queries | Three models above | Critical | S |
| P11-T02 | Job runner | A small `jobs.Run(ctx, name, fn)` wrapper: `pg_advisory_lock`, writes a `job_runs` row, captures errors, skips if already locked | Critical | S |
| P11-T03 | CMC client | `net/http` wrapper: 10s timeout, retry on 5xx with backoff, on 429 stop the cycle and log; typed response structs | Critical | M |
| P11-T04 | Coin map sync | Daily `robfig/cron` job + run on boot if `market_instruments` is empty | High | S |
| P11-T05 | Price refresh job | Distinct instrument_ids from non-deleted AUTO crypto investments → batched quotes → upsert market_prices | Critical | M |
| P11-T06 | Link investments to instruments | On create/update with AUTO crypto: require `instrumentId` from search, or resolve the symbol to the highest-ranked match and return it; trigger an immediate price fetch for new instruments | Critical | M |
| P11-T07 | Valuation integration | Use market_prices when AUTO; `isStale` if as_of is older than 30 minutes; keep the last known price on provider failure | Critical | S |

**Acceptance criteria:** creating AUTO BTC shows a market price within one refresh; a provider outage keeps the last price with `isStale: true` and a FAILED job_runs row; no API key appears in logs or responses; a single cycle costs ⌈coins/100⌉ calls.

**Tests:** CMC client against recorded fixtures (`httpmock`); job tests with a mocked client; e2e for search and valuation with AUTO mode.

---

### P12: US stock / ETF auto pricing (Massive)

**Goal:** Ticker search plus prices for AUTO US_STOCK and US-listed GLOBAL_ETF investments, with an EOD strategy (free tier) and a snapshot strategy (paid tier).
**Depends on:** P11 · **Size:** Medium

**Massive usage** (Massive is the renamed Polygon.io; confirm paths and tier limits in the current docs)
- Ticker search: `GET /v3/reference/tickers?search=&market=stocks&active=true` (types CS and ETF) → upsert `market_instruments`.
- **EOD mode** (`MASSIVE_MODE=EOD`, free tier): `GET /v2/aggs/grouped/locale/us/market/stocks/{date}?adjusted=true`. One call returns every US ticker's daily bar; store only the referenced tickers. Run daily after the US close (~22:30 UTC). An empty result (weekend or holiday) falls back to the previous trading day, up to 5 days back.
- **Snapshot mode** (`MASSIVE_MODE=SNAPSHOT`, paid): `GET /v2/snapshot/locale/us/markets/stocks/tickers?tickers=AAPL,VOO,...` every 5 minutes during US market hours.
- Client-side token-bucket rate limiter (`golang.org/x/time/rate`, default 5 requests/min, configurable).
- Staleness: EOD → stale after 4 days; snapshot → stale after 30 minutes during market hours.

| Method | Path | Notes |
|---|---|---|
| GET | /market/stocks/search?q=&kind=STOCK\|ETF | checks local instruments first; calls Massive when there are fewer than 5 local hits; throttled |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P12-T01 | Massive client | `net/http` wrapper + token bucket; API key sent as a header, never logged | Critical | M |
| P12-T02 | Ticker search + instrument upsert | As above; cache results as market_instruments rows | High | M |
| P12-T03 | Price strategy interface | `marketdata.PriceStrategy` interface with `EodStrategy` and `SnapshotStrategy` implementations, selected by env | Critical | M |
| P12-T04 | Jobs | Schedules per strategy using the P11 job runner | Critical | S |
| P12-T05 | Investment linking | AUTO US_STOCK/GLOBAL_ETF requires an instrument_id (or resolves the exact ticker); immediate fetch on create (EOD uses the previous close) | Critical | S |
| P12-T06 | Valuation integration | Reuse P11 logic with provider-specific staleness | High | S |

**Acceptance criteria:** AAPL and VOO in AUTO mode show the last close in EOD mode; a weekend run uses Friday's close; switching `MASSIVE_MODE` needs no code change; the rate limiter prevents more than the configured calls per minute (unit test).

---

### P13: Investment buy/sell through wallets

**Goal:** Fund an investment from a wallet (BUY) and move sale proceeds into a wallet (SELL), updating the position. This supports "Initial position" funding on create (the reference shows "Posisi awal BTC/USDT" as a transfer).
**Depends on:** P9, P10 · **Size:** Medium

**Rules**
- **BUY** (`from: WALLET`, `to: {type: "INVESTMENT", investmentId, quantity, unitPrice, fee?}`).
  - Cost in quote currency = quantity × unit_price + fee. The wallet is debited in its own currency: if currencies differ, the client sends `amount` (wallet side) or the server converts with the P5 ConversionContext; the `fx_rate` used is stored.
  - Position: new_qty = q + Δq; new_avg = (q × avg + Δq × unit_price) / new_qty; fees += fee.
- **SELL** (`from: {type: "INVESTMENT", investmentId, quantity, unitPrice, fee?}`, `to: WALLET`).
  - Δq ≤ quantity (`INSUFFICIENT_QUANTITY`; the staked quantity rule was removed in feedback round 3). The average stays the same.
  - realized_pl = (unit_price − avg) × Δq − fee, stored on the transaction. The wallet is credited with the proceeds.
- Each BUY/SELL stores `position_before` and `position_after` (quantity, avg_buy_price, fees).
- **Edit:** allowed only if the investment's current position equals `position_after`, otherwise 409 `POSITION_CHANGED_SINCE`. **Delete** (feedback round 3): any trade can be deleted; the later trades are replayed on top of the deleted trade's `position_before`, refused with 422 `TRADE_DELETE_BREAKS_LATER_SELL` when a later SELL would not fit, and 409 `POSITION_CHANGED_SINCE` when the position was edited by hand. Reversal reverses the wallet effect.
- Shared investments (P14): only the owner can BUY/SELL.
- `POST /investments` accepts optional `funding: {walletId}`, which creates a BUY titled "Initial position {symbol}" in the same `pgx.Tx`.

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P13-T01 | Extend transfer contract | Enable INVESTMENT in the P9 discriminated `from`/`to` decode; reject INVESTMENT→INVESTMENT (`NOT_SUPPORTED`) | Critical | S |
| P13-T02 | Position math | Pure functions for buy and sell math + `SELECT ... FOR UPDATE` row lock | Critical | M |
| P13-T03 | BUY flow | Ledger debit + position update + transaction row, all in one `pgx.Tx` | Critical | M |
| P13-T04 | SELL flow | Ledger credit + quantity decrease + realized_pl | Critical | M |
| P13-T05 | Edit/delete with snapshot guard | As per the rules | High | M |
| P13-T06 | Funding on create | Extend P10 create | High | S |
| P13-T07 | Realized P/L in detail | `realizedPl` total on investment detail (sum of SELL transactions, converted) | Medium | S |

**Acceptance criteria:** buying 0.5 BTC at 60,000 and then 0.5 at 70,000 gives avg 65,000 and quantity 1; selling 0.4 at 80,000 with fee 10 records realizedPl 5,990; deleting that SELL restores quantity 1; editing an older BUY after a later SELL → 409; the wallet ledger invariant holds.

---

### P14: Shared investments

**Goal:** One investment co-owned by several people with percentage shares, where each person sees and counts only their share.
**Depends on:** P10, P13 · **Size:** Medium-Large

**Model and rules (D-06, D-07 recommendations applied)**
- The investment lives in the **owner's space**. Only the owner edits the investment, its price data, BUY/SELL, and the participant list.
- `investment_participants`: id, investment_id, role (`OWNER` / `PARTICIPANT`), user_id (nullable), display_name, email (nullable), share_bps (1 to 10,000), status (`ACTIVE` / `INVITED` / `DECLINED` / `REMOVED`), attached_space_id (nullable), invited_at, responded_at; unique (investment_id, user_id) where user_id is not null.
- `investment_share_changes`: id, investment_id, changed_by_id, before (JSONB), after (JSONB), created_at (audit trail).
- The API accepts `sharePct` as a string with up to 2 decimals and stores basis points. The sum of **all non-removed participants must be exactly 100.00%** (`SHARE_TOTAL_INVALID`). The owner must be included with a share above 0. Maximum 10 participants.
- Participant kinds:
  - **Email participant** → `INVITED`. To prevent account enumeration, the response is the same whether or not the email is registered. When a user with that email exists (or registers later), they see the invitation and can accept or decline.
  - **Name-only participant** → `ACTIVE` immediately; just a label with a share (someone without an account).
- Shares are allocated regardless of acceptance status: an invited person's share is still theirs and is never counted for the owner.
- **Allocation:** value, cost basis, unrealized and realized P/L are each split by share_bps using the largest-remainder method at the base currency's minor units, so the parts always add up exactly to the total.
- **Net worth:** the owner's space counts the owner's share (`ownershipFactor` = owner_bps / 10,000). A participant who accepts attaches it to one of their spaces, which counts their share.
- **Privacy for participants:** they see the investment name, type, quantity, prices, valuation, and participants' display names and shares. They do **not** see the owner's space, other assets, or other participants' emails (masked).
- Removing sharing (`DELETE participants`) returns 100% to the owner; participants become REMOVED. Deleting the investment hides it for everyone.

| Method | Path | Who |
|---|---|---|
| GET | /spaces/{spaceId}/investments/{id}/participants | owner |
| PUT | /spaces/{spaceId}/investments/{id}/participants | owner; full replacement list `[{participantId?, email?, displayName, sharePct}]` |
| DELETE | /spaces/{spaceId}/investments/{id}/participants | owner; stop sharing |
| GET | /me/shared-investment-invitations | invitee |
| POST | /me/shared-investment-invitations/{participantId}/accept | invitee; `{spaceId}` (must be a member of that space) |
| POST | /me/shared-investment-invitations/{participantId}/decline | invitee |
| POST | /me/shared-investments/{participantId}/leave | participant; their share stays allocated but becomes a name-only participant (owner notified via audit) |
| GET | /spaces/{spaceId}/investments?include=shared | extends P10: shared-in items flagged `ownership: "SHARED_IN"`, read-only, with `myShareBps` and valuation for my share plus the total |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P14-T01 | Migration + sqlc queries | Two models above; `investments.is_shared` maintained by the handler | Critical | S |
| P14-T02 | Allocation function | `allocation.Split(total decimal.Decimal, shares []int, minorUnits int) []decimal.Decimal` with largest remainder; property-based table-driven tests (sum always equals total) | Critical | S |
| P14-T03 | PUT participants | Validate the sum, owner presence, limit; diff against existing rows; write the audit row; all in one `pgx.Tx` | Critical | M |
| P14-T04 | Invitation inbox, accept, decline, leave | Match by user_id or by verified account email (case-insensitive) | Critical | M |
| P14-T05 | ownershipFactor in valuation | Owner space → owner_bps; attached space → participant_bps; the P10 summary uses my-share values | Critical | M |
| P14-T06 | Shared-in listing + read access guard | A participant may GET only via the shared-in listing; any write → 404 | Critical | M |
| P14-T07 | Owner-only enforcement | BUY/SELL/PATCH on shared investments limited to the owner | High | S |

**Acceptance criteria**
- 3 people at 50 / 30 / 20 on a total value of 1,000.01 USD → 500.01 / 300.00 / 200.00 (sums exactly).
- Owner space net worth includes 50% only; the participant's attached space includes 30% after acceptance.
- An unregistered email invite responds identically to a registered one; after that email registers, the invitation appears.
- Shares summing to 99.99 → 422; a participant attempting PATCH → 404.

---

### P15: Other assets & net-worth summary

**Goal:** Fixed assets, money owed (receivables), and debt (liabilities) with settlement payments, plus the asset-page summary.
**Depends on:** P9, P14 · **Size:** Medium

**Data model**
- `manual_assets`: id, space_id, kind (`FIXED_ASSET` / `RECEIVABLE` / `LIABILITY`), name, icon, currency, current_value (for a liability this is the outstanding amount), counterparty_name (nullable), due_date (nullable), estimated_annual_change_pct (nullable, fixed assets), note, created_by_id, created_at, updated_at, deleted_at.
- `manual_asset_value_changes`: id, asset_id, previous_value, new_value, reason (`MANUAL` / `SETTLEMENT`), transaction_id (nullable), changed_at.

**Settlements** (the transfer icons on the Money owed and Debt cards): `POST .../manual-assets/{id}/settlements {walletId, amount, occurredAt?}`
- RECEIVABLE: wallet += amount; asset value −= amount (money received back).
- LIABILITY: wallet −= amount; outstanding −= amount (debt payment).
- Recorded as a TRANSFER transaction with `manual_asset_id` set. Amount must not exceed current_value (`SETTLEMENT_EXCEEDS_BALANCE`). Same currency only in v1.

| Method | Path |
|---|---|
| GET/POST | /spaces/{spaceId}/manual-assets?kind= |
| GET/PATCH/DELETE | /spaces/{spaceId}/manual-assets/{id} |
| POST | /spaces/{spaceId}/manual-assets/{id}/settlements |
| GET | /spaces/{spaceId}/net-worth/summary |

**Summary response:** `{currency, cash, investments: {value, unrealizedPlPct}, fixedAssets, receivables, liabilities, netWorth, warnings[]}`, where netWorth = cash + investments (my share) + fixedAssets + receivables − liabilities. In `CASH_ONLY` tracking mode (D-13), only `cash` and `netWorth = cash` are returned.

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P15-T01 | Migration + sqlc queries | Models above | Critical | S |
| P15-T02 | CRUD + value history | Every value change writes a manual_asset_value_changes row | Critical | M |
| P15-T03 | Settlements | Ledger + asset value update + transaction, all in one `pgx.Tx`; deleting the transaction reverses both | High | M |
| P15-T04 | Net-worth summary function | `networth.Compute(ctx, spaceID, at *time.Time)`, reused by P17 snapshots | Critical | M |

**Acceptance criteria:** a debt of 500 with a 200 payment → outstanding 300 and wallet −200; the summary matches a hand-computed fixture across currencies; CASH_ONLY hides the other sections.

---

### P16: Goals

**Goal:** Savings goals funded by earmarking ("reserving") part of wallet balances. No money moves.
**Depends on:** P7 · **Size:** Small-Medium

**Data model**
- `goals`: id, space_id, name, icon, color, target_amount, currency (default space base), target_date, achieved_at (nullable), created_by_id, created_at, updated_at, deleted_at.
- `goal_allocations`: id, goal_id, wallet_id, amount (wallet currency), updated_at; unique (goal_id, wallet_id).

**Rules**
- `wallet.reserved` = Σ allocations; `wallet.available` = balance − reserved (the "Available: $134.26" text).
- Setting an allocation requires amount ≤ available + this goal's current allocation on that wallet (`INSUFFICIENT_AVAILABLE_BALANCE`).
- Expenses may push a wallet below its reserves → `overReserved: true` (D-09).
- Progress = Σ allocations converted to goal currency. `achieved_at` is set when progress first reaches the target and cleared if it drops below.
- `requiredMonthly` = remaining / months until target_date (null if past or achieved).
- Deleting a wallet releases its allocations. Deleting a goal releases all its allocations.

| Method | Path |
|---|---|
| GET/POST | /spaces/{spaceId}/goals (POST accepts optional `initialReserve {walletId, amount}`) |
| GET/PATCH/DELETE | /spaces/{spaceId}/goals/{id} |
| PUT | /spaces/{spaceId}/goals/{id}/allocations/{walletId} `{amount}` (0 removes) |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P16-T01 | Migration + sqlc queries | Models above | Critical | S |
| P16-T02 | Goal CRUD + progress | Progress computed on read via the P5 aggregator | Critical | M |
| P16-T03 | Allocations | Validation under `SELECT ... FOR UPDATE` on the wallet | Critical | M |
| P16-T04 | Wallet integration | Add `reserved`, `available`, `overReserved` to P7 wallet responses; release on wallet delete | High | S |

**Acceptance criteria:** a wallet of 134.26 with a 100 reservation shows available 34.26; reserving 50 more → 422; an expense of 60 → `overReserved: true`; progress converts correctly across currencies.

---

### P17: Dashboard, analytics & net-worth history

**Goal:** Feed the home screen (overview, category breakdown, weekly analytics, calendar) and the net-worth graph.
**Depends on:** P8, P15 · **Size:** Medium

**Data model:** `net_worth_snapshots`: space_id, date, currency, cash, investments, fixed_assets, receivables, liabilities, net_worth, created_at; unique (space_id, date).

| Method | Path | Response |
|---|---|---|
| GET | /spaces/{spaceId}/dashboard/overview?period= | income, expense, transactionCount, and previous-period values with % change (null when the previous value is 0) |
| GET | /spaces/{spaceId}/dashboard/category-breakdown?period=&type= | per category: amount, pct, count; plus uncategorized |
| GET | /spaces/{spaceId}/dashboard/analytics?period= | buckets: for month-length periods, days 1–7, 8–14, 15–21, 22–28, 29–end (as in the screenshot); for THIS_YEAR, monthly; each bucket has income and expense |
| GET | /spaces/{spaceId}/dashboard/calendar?month=YYYY-MM | per local day: income, expense, net, count |
| GET | /spaces/{spaceId}/net-worth/history?range=1M\|3M\|6M\|1Y\|ALL | points `[{date, netWorth}]` + a live point for today + changePct over the range |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P17-T01 | Migration + sqlc queries | Snapshot model | Critical | S |
| P17-T02 | Overview + breakdown | SQL aggregation by currency → the P5 aggregator; previous period from the P8 PeriodResolver | Critical | M |
| P17-T03 | Analytics buckets | Bucket builder (pure, table-driven unit tests) + grouped SQL | High | M |
| P17-T04 | Calendar | Group by user-local date for the month | High | S |
| P17-T05 | Snapshot job | Daily 00:15 UTC via the P11 job runner: `networth.Compute` for each active space → upsert today's UTC-date row (TD-04) | Critical | M |
| P17-T06 | History endpoint | Downsample to at most ~120 points; append the live point | High | S |

**Acceptance criteria:** overview totals equal the sum of transactions in the period (transfers and adjustments excluded); November buckets end at "29–30 Nov"; a space created today returns one live point; the job is idempotent per day.

---

### P18: Investment accounts (securities & exchanges)

**Goal:** Group investments under a wrapper, a *securities* account for stocks (e.g. a brokerage) or an *exchange* account for crypto, so one wrapper can hold many instruments, show combined totals, and be shared as a whole. Sharing of individual investments (P14) stays unchanged.
**Depends on:** P10, P13, P14 · **Size:** Medium
**Added after P17 at the user's request; Hardening moved from P18 to P19 and every reference was updated.**

**Data model:**
- `investment_accounts`: id, space_id, name, kind (`SECURITIES` | `EXCHANGE`), is_shared, archived_at, created_at, updated_at; unique (space_id, lower(name)).
- `investments.account_id` (nullable FK). Existing investments stay ungrouped. Kind rule: `SECURITIES` holds `ID_STOCK` / `US_STOCK`; `EXCHANGE` holds `CRYPTO`.
- Account-level sharing reuses the P14 model, keyed by account (`investment_account_participants`, `investment_account_share_changes`), with the same rules: owner present, shares sum to 10000 bps, owner-only writes, audit rows, invitations.

| Method | Path | Notes |
|---|---|---|
| POST / GET | /spaces/{spaceId}/investment-accounts | create; list with value, cost, unrealized/realized P/L, holdings count (converted, my-share) |
| GET / PATCH / DELETE | /spaces/{spaceId}/investment-accounts/{id} | DELETE only when empty (409 `ACCOUNT_NOT_EMPTY`); otherwise archive |
| PUT / DELETE | /spaces/{spaceId}/investment-accounts/{id}/participants | as P14 |
| GET/POST | invitation inbox, accept, decline, leave | as P14, for accounts |
| POST/PATCH | /spaces/{spaceId}/investments | accept optional `accountId`; kind mismatch → 422 `ACCOUNT_KIND_MISMATCH` |
| GET | /spaces/{spaceId}/investments?accountId= | filter; `accountId=none` for ungrouped |

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P18-T01 | Migration + sqlc queries | Tables above; `investments.account_id` index | Critical | S |
| P18-T02 | Account CRUD + kind validation | `internal/investmentaccounts`; archive vs delete | Critical | M |
| P18-T03 | Assign/move investments | `accountId` on create/PATCH; refuse moving an investment that has its own participants into a shared account (409 `INVESTMENT_HAS_PARTICIPANTS`) | Critical | M |
| P18-T04 | Account valuation | Sum holdings with the P10 `Value`/`Summarize` and the P5 aggregator; per-account totals and P/L | Critical | M |
| P18-T05 | Account-level sharing | Reuse `allocation.Split` and the P14 participant/invitation flow keyed by account; holdings inherit the account's shares (effective share resolved in one place) | Critical | L |
| P18-T06 | Net worth and allocation integration | `networth.Compute`, `Allocate`, `AllocateRealized`, `include=shared` use the effective shares; no double counting | Critical | M |
| P18-T07 | Tests | Unit (effective share resolution) + e2e: kind rules, totals, move in/out, shared account visible to a participant, non-member 404, ledger invariant unaffected | Critical | M |

**Open decisions (proposed defaults, confirm before implementation):**
1. Overlapping shares: holdings inside an account take the account's shares (D-15); ungrouped investments keep individual sharing.
2. Moving an investment with its own participants into a shared account is blocked until they are removed.
3. Kinds limited to `SECURITIES` and `EXCHANGE`; a securities account holds both ID and US stocks.
4. Accounts hold no cash balance; buys and sells stay funded from wallets.
5. Existing investments stay ungrouped until assigned (no auto-migration).

**Acceptance criteria:** a crypto account holding BTC and ETH reports their combined value and P/L; adding a stock to it → 422; sharing an account 50/50 gives the participant 50% of every holding (including ones added later) in their net worth and allocation; individual investment sharing still works; a non-member gets 404.

---

### P19: Hardening, docs & deployment

**Goal:** Production readiness.
**Depends on:** all phases · **Size:** Medium

| ID | Task | Technical approach | Pri | Size |
|---|---|---|---|---|
| P19-T01 | Isolation sweep | An e2e test that walks the chi route tree and asserts 404 for a non-member on every `/spaces/{spaceId}` route | Critical | M |
| P19-T02 | Security review | Checklist: CORS allowlist, cookie flags, security headers middleware, body limits, parameterized sqlc queries (no hand-built SQL strings), no secrets in logs, argon2id parameters, JWT secret rotation procedure documented | Critical | M |
| P19-T03 | Rate-limit tuning | Global 120/min per user; stricter limits on auth and search | High | S |
| P19-T04 | OpenAPI completeness | `swag` examples on every handler; `make swagger` → `openapi.yaml` for frontend client generation | High | S |
| P19-T05 | Dockerfile | Multi-stage build (Go build stage → distroless/alpine runtime), non-root user, `goose up` as a release step (or an init container/entrypoint step — decide here), healthcheck hitting `/health` | Critical | S |
| P19-T06 | CI pipeline | GitHub Actions: `go vet`, `golangci-lint`, unit tests, e2e tests with a Postgres service container, build and push the image | Critical | S |
| P19-T07 | Observability | zerolog JSON output, Sentry (optional, `getsentry/sentry-go`), a small script to query recent `job_runs`, failed-job alert via log-based alert | Medium | S |
| P19-T08 | Seed/demo data | `make seed-demo` (a small Go command) creating a user with wallets, transactions, investments, a shared investment, and goals | Medium | S |
| P19-T09 | Deployment runbook | Environments, env vars, backups (managed DB point-in-time recovery), rollback steps | High | S |

**Acceptance criteria:** CI is green on main; the container starts with only env vars; the isolation sweep covers 100% of space routes; the runbook has been followed once on staging.

---

## 9. Risks, technical debt and open issues

| ID | Type | Description | Mitigation |
|---|---|---|---|
| R-01 | Risk | CMC and Massive free tiers have strict limits and may change | Batched calls, configurable intervals, EOD strategy, stale-price fallback; confirm plans before P11/P12 |
| R-02 | Risk | No auto price for Indonesian stocks and mutual funds | Manual pricing in v1; add an IDX/NAV provider later behind the same `PriceStrategy` interface |
| R-03 | Risk | Multi-currency edge cases (missing rates) | Warnings instead of failures; daily FX job; manual rates |
| R-04 | Risk | Shared investments: percentages fixed while contributions vary over time | v1 uses explicit percentages only; per-contribution capital accounting is a future enhancement |
| R-05 | Risk | The position snapshot model limits edits to older BUY/SELL transactions | `POSITION_CHANGED_SINCE` guard; a lot-based ledger is a future enhancement |
| R-06 | Risk | sqlc generated-code workflow needs a team decision: commit generated Go to the repo, or regenerate in CI on every build | Decide explicitly in P1-T04; commit is simpler for a solo project and keeps builds reproducible without the sqlc binary at deploy time |
| TD-01 | Debt | No Redis: in-memory rate limiting and a single instance | Add Redis (rate-limit storage + a real queue) before scaling horizontally |
| TD-02 | Debt | Cached wallet balances can drift if code bypasses the `ledger` package | Single-writer rule, invariant tests, `RecomputeBalance` script |
| TD-03 | Debt | ILIKE search will slow down at scale | Add a `pg_trgm` index when transaction counts grow |
| TD-04 | Debt | Snapshots use UTC dates, not each space's local date | Add a space timezone later if needed |
| TD-05 | Debt | Cross-currency settlements and goal allocations are limited in v1 | Revisit after v1 |

---

## Appendix A: Coding-agent brief template

```
You are implementing Phase <Pn> of the backend described in docs/backend-plan.md.

CONTEXT
- Read Sections 1 to 6 (product, decisions, stack, architecture, conventions, data model) and Section 8 Phase <Pn>.
- Phases <list> are complete. Do not change their public contracts unless Phase <Pn> says so.
- Stack: Go, chi, sqlc, pgx, goose, PostgreSQL. Money is always shopspring/decimal.Decimal, never float64.

OBJECTIVE
<copy the phase goal>

SCOPE
Implement only tasks <Pn-T01..Tnn>.

OUT OF SCOPE
Anything in later phases. Do not add libraries that are not listed in Section 3 without explaining why in your report.

TECHNICAL REQUIREMENTS
Follow Section 5 conventions: money as strings/NUMERIC/decimal.Decimal, the apperror envelope, multi-tenancy
via explicit space_id filters, the PeriodResolver, the ledger package as the only balance writer, and the
Definition of Done.

STEPS
1. goose migration + sqlc queries, then `make sqlc-gen`. 2. Pure logic with table-driven unit tests.
3. HTTP handlers + validation. 4. e2e tests (happy path, validation, unauthenticated, cross-space 404, business
rules). 5. swag annotations and Appendix B.

ACCEPTANCE CRITERIA
<copy from the phase>

DELIVERABLES (report back)
- Files created or changed
- Endpoints added, with example request and response
- Migration file name(s)
- Output of `go vet`, `golangci-lint run`, `go test ./...`, and `go test -tags=e2e ./...`
- Deviations from the plan and the reasons
- Open questions and any technical debt introduced
```

---

## Appendix B: Error code catalog (initial)

| Code | HTTP | Phase |
|---|---|---|
| VALIDATION_FAILED | 400 | P1 |
| UNAUTHENTICATED | 401 | P2 |
| FORBIDDEN | 403 | P3 |
| NOT_FOUND / <ENTITY>_NOT_FOUND | 404 | all |
| RATE_LIMITED | 429 | P1 |
| METHOD_NOT_ALLOWED | 405 | P1 |
| PAYLOAD_TOO_LARGE | 413 | P1 |
| DATABASE_UNAVAILABLE | 503 (readiness probe) | P1 |
| INTERNAL_ERROR | 500 | P1 |
| EMAIL_ALREADY_REGISTERED | 409 | P2 |
| INVALID_CREDENTIALS | 401 | P2 |
| REFRESH_TOKEN_REUSED / REFRESH_TOKEN_INVALID | 401 | P2 |
| RESET_TOKEN_INVALID | 422 | P2 |
| INVALID_CURRENT_PASSWORD | 422 | P2 |
| OWNERSHIP_TRANSFER_REQUIRED, LAST_SPACE, OWNER_CANNOT_LEAVE | 422 | P3 |
| INVITATION_INVALID, INVITATION_EXPIRED, ALREADY_A_MEMBER, SPACE_MEMBER_LIMIT | 422/409 | P3 |
| APP_LOCK_TEMPORARILY_LOCKED, APP_LOCK_NOT_ENABLED | 429/422 | P4 |
| APP_LOCK_ALREADY_ENABLED | 409 | P4 |
| INVALID_PIN | 422 | P4 |
| FX_RATE_UNAVAILABLE | 422 (internal; normally surfaced as a warning) | P5 |
| CATEGORY_NAME_TAKEN, BUDGET_NOT_ALLOWED, CATEGORY_TYPE_LOCKED, CATEGORY_TYPE_MISMATCH | 409/422 | P6, P8 |
| WALLET_CURRENCY_LOCKED, WALLET_HAS_BALANCE | 422 | P7 |
| WALLET_HAS_TRANSACTIONS | 409 (details: transactions, transfers, goalReserves; `?force=true` overrides) | Feedback round 2, Phase I |
| USE_SPECIFIC_ENDPOINT | 422 | P8 |
| TYPE_CHANGE_NOT_ALLOWED | 422 | Feedback round 2, Phase F |
| CATEGORY_EXISTS_SIMILAR | 409 (details.existingId; `?force=true` overrides) | Feedback round 2, Phase F |
| TRANSFER_SAME_WALLET, TRANSFER_AMOUNT_REQUIRED, NOT_SUPPORTED_YET | 422 | P9 |
| AUTO_PRICE_NOT_SUPPORTED | 422 | P10 |
| UPSTREAM_PROVIDER_ERROR | 502 | P11, P12 |
| INSTRUMENT_NOT_FOUND | 422 | P11 |
| INSTRUMENT_NOT_LINKED, PRICE_MODE_NOT_AUTO | 422 | P11 |
| INSUFFICIENT_QUANTITY, POSITION_CHANGED_SINCE | 422/409 | P13 |
| TRADE_DELETE_BREAKS_LATER_SELL | 422 | feedback round 3, Phase N |
| SHARE_TOTAL_INVALID, PARTICIPANT_LIMIT, OWNER_ONLY | 422/403 | P14 |
| SHARE_INVITATION_NOT_FOUND | 404 | P14 |
| SHARE_INVITATION_ANSWERED | 409 | P14 |
| SHARE_OWNER_CANNOT_LEAVE | 422 | P14 |
| SETTLEMENT_EXCEEDS_BALANCE | 422 | P15 |
| SETTLEMENT_CURRENCY_MISMATCH, SETTLEMENT_NOT_SUPPORTED, MANUAL_ASSET_CURRENCY_LOCKED | 422 | P15 |
| INSUFFICIENT_AVAILABLE_BALANCE | 422 | P16 |
| ALLOCATION_EXCEEDS_100 | 422 | feedback phase C |
| WRAPPER_NOT_FOUND | 404 | feedback phase C |

---

## Appendix C: Screenshot-to-phase traceability

| Screenshot | Backend phases |
|---|---|
| login | P2 (email/password replaces Google/Apple) |
| home | P8, P17 |
| transaction_page, filter_history | P8, P9 |
| bank_list, bank_detail, quick_edit_bank, bank_create | P7, P8, P16 |
| solid_edit_bank_card, gradation_edit_bank_card | P7 (card_style) |
| bank_transfer | P9, P13 |
| asset_page, portofolio | P10, P14, P15, P17 |
| crypto_create | P10, P11 |
| us_stock_create, global_etf_create | P10, P12 |
| indo_stock_create, mutual_fund_create | P10 (manual) |
| expense_list, ncome_list, expense_income_category | P6, P8 |
| goal, add_goal | P16 |
| settings, lock_n_hide | P3, P4, P5 |
