# Feedback round 2: backend plan (data correctness, crypto pricing, currency, small API gaps)

Nov 23, 2026 · @jo

Scope: backend only (Go, chi, sqlc, goose, Postgres). Nothing here is implemented yet. Paths are relative to `/home/jonathanr/code/hzzt`. API base is `/api/v1`. Error envelope is `httpx.ErrorEnvelope` with `apperror` codes (see `plan/plan.md` Appendix B). This follows `plan/feedback-be-plan.md` (Phases A to E, done). It does not replace it. The frontend counterpart is `../hzzt-fe/plan/feedback-fe-8-plan.md`.

**Verification limits.** The feedback is text and screenshots only, so every root cause below is **inferred** from reading the code, and marked "verify" where a query or a row must be looked at first. Each phase starts with a failing test (or a SQL check against a dev database) that proves the cause before the fix is written.

**Secrets.** The CoinMarketCap, Massive and FreeCurrency keys live in `.env` only (`CMC_API_KEY`, `MASSIVE_API_KEY`, `FREECURRENCY_API_KEY`). They are never written into a plan, a test, a log line or a commit.

## 0. Summary of findings

Feedback items are the review rows T1 to T18 plus four loose requests (L1 to L4). Only the ones that need backend work are listed here. The rest are frontend only (see the frontend plan, section 1).

| # | Item | Finding |
|---|---|---|
| T1 | Opening balance counted as income | The dashboard query already sums only `INCOME` and `EXPENSE` (`internal/db/queries/dashboard.sql:5`), and real `OPENING_BALANCE` rows are excluded. The leak is elsewhere: `transactions.TopUp` creates an **`INCOME`** titled "Balance top up" (`internal/transactions/service.go:161`), so every top up inflates income and the "+100% vs previous period". "Saldo awal" ($15.67) is not a string the backend writes (it writes "Opening balance"), so it is most likely an `INCOME` the user typed by hand. **Verify** with `SELECT type, title, count(*) ... GROUP BY 1,2` on the affected space |
| T2 | Mixed types summed in the calendar | `Calendar` puts every row that is not `INCOME` into `exp` (`dashboard/service.go:362`). If `dayRows` ever returns another type the bucket is polluted. `Day` has no `net` field, so a client that wants one has to add income and expense itself. **Verify** the `dayRows` SQL type filter. The "374" in the review is the sum of an income-like total and a $224.52 investment position, so it is also a client bug (frontend plan F33) |
| T3 | Net worth +20.7% while investments -32% | `networth.HistoryDTO.changePct` is first-to-last of the **value**, so a top up or a deposit counts as growth. There is no figure that separates contributions from market change. The per-investment P/L is against cost, so the two percentages measure different things |
| T4 | Currency handling | Each investment already stores its own `quote_currency`, fixed by asset type for four of five types (`types.go:96-130`). The manual "USD/IDR rate" in the forms maps to `manualFxRate` (`inputs.go:33`). The backend already has an FX provider (`internal/currency`, `GET /fx/latest`, a daily job), so a manual rate is only needed as an override, not as the only way |
| T7 | Wallet delete has no impact count | `DELETE` has no way to tell the client how many transactions go with the wallet |
| T8 | Silent 5.5% annual change | `estimated_annual_change_pct numeric(8,4) NOT NULL DEFAULT 5.5` (`00008_investments.sql:19`). Every investment is created with an assumed return the user never typed |
| T12 | Mixed languages, duplicate categories | System rows are written in English ("Opening balance", "Balance top up", "Balance adjustment") and the client has no key to localize them. Seeds are English only. The unique index is per type and name (`00006_categories.sql:20`), so "Gaji" and "Salary" are legal duplicates in meaning |
| T13 | Same default icon | The `Subscriptions` seed uses the same sparkle emoji that the client uses as its fallback (`categories/seeds.go:25`) |
| T14 | Filter multi-select | **Verify** whether `GET /spaces/{id}/transactions` takes several category ids. `types` is already multi-value (`transactions/handlers.go:327`) |
| L1 | Crypto in the investments total uses the CMC price | The pricing engine exists (`marketdata`, `coinmarketcap/client.go`, refresh every `CMC_REFRESH_MINUTES`, valuation uses the market price when `price_mode = AUTO`). But `price_mode` defaults to `MANUAL` (`00008_investments.sql:6`, `inputs.go:366`) and the create form sends `MANUAL` by default (frontend `CreateInvestmentScreen.tsx:204`). So a crypto holding is only priced by CMC if the user switched it to Auto and picked an instrument. Everything else is valued at the price typed once. Also `manual_current_value` beats price in `Value` (`valuation.go:130`). See Phase G |
| L2 to L4 | Assets dropdown, Home goals button, goals graphic | Frontend only |

## 1. Phase F: Ledger correctness (T1, T2, T3, T12 system keys)

### 1.1 Top up must not be income (T1)

Decision D-B1 (proposed, recommended): `POST /wallets/{id}/top-up` records an **`ADJUSTMENT` with `direction = IN`** instead of an `INCOME`. This matches what the wallet edit sheet already tells the user ("recorded as adjustment transactions"), and the dashboard, the calendar and the savings rate already ignore `ADJUSTMENT`. The route, the request and the response shape stay the same (`TransactionDTO`), so the client does not break. Only `type` in the response changes from `INCOME` to `ADJUSTMENT`.

- `transactions.TopUp` calls the existing `wallets.writeBalanceTx` path (or the ledger helper behind it) with `ledger.TypeAdjustment`, `IN`, title key `BALANCE_TOP_UP`. No category.
- The wallet balance maths is unchanged (`ledger.sql` already adds `ADJUSTMENT IN`).
- Migration `00017_topup_to_adjustment.sql`: reclassify existing rows: `UPDATE transactions SET type='ADJUSTMENT', direction='IN' WHERE type='INCOME' AND category_id IS NULL AND title = 'Balance top up' AND deleted_at IS NULL`. The `CHECK ((type IN ('ADJUSTMENT','OPENING_BALANCE')) = (direction IS NOT NULL))` constraint (`00007:59`) is satisfied by setting both in one statement. Down migration reverses only rows it changed (tag them with a temporary `note` marker or keep an audit table). **Verify** the row count first and print it in the migration log.
- Net worth snapshots and history are computed from balances, not from transaction types, so they do not change. Recompute is not needed.

### 1.2 Reclassify a hand-typed opening balance (T1, "Saldo awal")

The backend cannot know that a user-typed income titled "Saldo awal" is really an opening balance, and a title match would be a guess. Decision D-B2 (proposed): allow a user to change one transaction's kind explicitly.

- `PATCH /spaces/{spaceId}/transactions/{id}` accepts `type` only as `"ADJUSTMENT"` (direction is taken from the old type: `INCOME` becomes `IN`, `EXPENSE` becomes `OUT`) and only for a transaction with **no category**. Anything else returns `422 USE_SPECIFIC_ENDPOINT` or `422 TYPE_CHANGE_NOT_ALLOWED` (new code, add to Appendix B).
- Wallet balance is unchanged by the reclassification (same sign, same amount), so no ledger reversal is needed. The change and its audit entry run in one transaction.
- No automatic title matching. The frontend offers this as a "Not income" action on an uncategorised income (frontend F33).

### 1.3 Calendar and day totals (T2)

- Add `net` to `dashboard.Day` and `CalendarDayDTO`: `income - expense` for that day, base currency, 2dp via `money.Amount`. Existing `income`, `expense`, `count` stay (non-breaking).
- Make the type explicit: in `Calendar`, `switch row.Type { case typeIncome: ...; case typeExpense: ...; default: continue }`. Today the `else` takes any other type as an expense.
- **Verify** `DailySummaryRows` filters on `type IN ('INCOME','EXPENSE')`. If it does not, add the filter in `internal/db/queries/dashboard.sql` and run `sqlc generate`.
- Same rule for `Analytics` day buckets (`dashboard/service.go:271`): transfers, trades, adjustments and opening balances never enter a bucket. Add a test per type.

### 1.4 Net worth: contributions vs market change (T3)

The graph must not call a deposit growth. Additive fields on `networth.HistoryDTO` (non-breaking):

- `netContributions` (string money): the sum of `ADJUSTMENT IN/OUT`, `OPENING_BALANCE`, and top ups (now adjustments) between the first and the last point, converted to the base currency with the snapshot FX. Transfers between the user's own wallets are 0 by construction.
- `marketChange` (string money): `(last - first) - netContributions`.
- `performancePct` (string or null): `marketChange / (first + netContributions)`; null when the denominator is 0 or negative.
- `changePct` stays as it is (first to last of the value). The frontend decides which one to headline (frontend F33, D-F44).

Open point: the contribution sum needs the base-currency amount per transaction. If `transactions` has no stored base amount, convert with the snapshot-date rate through `currency.ConversionContext` (same rule as `dashboard.totals`) and add a warning for any missing rate (same `warnings` shape).

### 1.5 System keys for localization (T12)

- Add `systemKey` (string or null) to `TransactionDTO`: `OPENING_BALANCE`, `BALANCE_TOP_UP`, `BALANCE_ADJUSTMENT`, or null for user transactions. It is derived from `type` and the stored title at read time, so no migration is needed. The stored `title` stays in English as a fallback.
- The client shows `t('tx.' + systemKey)` when `systemKey` is set and the user's language is not English. Nothing here translates text on the server.

### 1.6 Seeds and de-duplication (T12, T13)

- `DefaultSeeds` get a `key` (stable, English: `SALARY`, `SUBSCRIPTIONS`, ...) stored in a new nullable `categories.system_key` column (migration `00018_category_system_key.sql`, backfill by matching the 11 seed names case-insensitively, per space). The client localizes by key. A user rename never changes the key.
- Seed locale: if a language is stored on the user or the space (**verify**; `grep locale|language` finds nothing in `users` or `spaces` today), seed the localized names at space creation. Otherwise leave names English and rely on `system_key`. Decision D-B3 below.
- Duplicate check on create and rename: `POST/PATCH categories` returns `409 CATEGORY_EXISTS_SIMILAR` (new code) with `{ "existingId": ... }` when the name matches a **seed alias** of the same type (small static map, `Gaji` and `Salary`, `Gaji` and `Pendapatan`...), on top of the existing case-insensitive unique index. A `force=true` query parameter overrides it. Keep the alias map in `categories/aliases.go` (id and en only).
- Seed icons: `Subscriptions` becomes a repeat/refresh symbol, not the sparkle. Add a test that all 11 seed icons are unique. Migration updates existing default rows only where `icon = '✨'` **and** `system_key = 'SUBSCRIPTIONS'`.

### Tests (Phase F)

Unit and e2e: top up is `ADJUSTMENT IN`, not in overview income, not in `Calendar`/`Analytics`; balance still rises; reclassify happy path and the four rejections (has category, transfer, already adjustment, wrong target); migration test on a seeded database (rows moved, balances unchanged, down works); calendar `net`; history `netContributions`/`marketChange`/`performancePct` for a top up (market change 0) and for a price move (contribution 0); `systemKey` on the three system kinds; seed icon uniqueness; alias conflict 409 and `force`.

Acceptance: `make lint test test-e2e`; swagger regenerated; Appendix B updated with `TYPE_CHANGE_NOT_ALLOWED` and `CATEGORY_EXISTS_SIMILAR`.

## 2. Phase G: Crypto priced by CMC in the investments total (L1)

### Current state
- Prices: `marketdata` refreshes AUTO crypto instruments every `CMC_REFRESH_MINUTES` (default 10, `config.go:154`) through `coinmarketcap.Client` (header `X-CMC_PRO_API_KEY`). `Value` uses `mp.Price` when `inv.PriceMode == AUTO && mp != nil` (`valuation.go:116`).
- The leak: `price_mode` defaults to `MANUAL`, `instrument_id` is only linked when the client sends one, and `manual_current_value` overrides `quantity × price` (`valuation.go:130`). So a crypto row created with defaults never touches CMC and the total keeps the price from the day it was typed.

### Design
Decision D-B4 (proposed, recommended): a `CRYPTO` investment is **AUTO by default** when its symbol resolves to a CMC instrument.

- On create with `assetType = CRYPTO`: if `priceMode` is not sent, set `AUTO` and link the instrument by symbol through the existing search/lookup (`marketdata.Search`, `service.go:455` logic). If the symbol is ambiguous (several CMC entries) or not found, fall back to `MANUAL` and return a `warnings[]` item `PRICE_NOT_LINKED` instead of failing. An explicit `priceMode` from the client always wins.
- Backfill migration `00019_crypto_auto_price.sql` (only after the user answers Q5): `UPDATE investments SET price_mode='AUTO' WHERE asset_type='CRYPTO' AND instrument_id IS NOT NULL AND manual_current_value IS NULL AND deleted_at IS NULL`. Rows without an instrument are listed in the migration log, not changed.
- `manual_current_value` on an AUTO crypto: keep the current rule (the manual value wins) but make it visible: `priceSource = MANUAL_VALUE` is already returned, and add a warning `MANUAL_VALUE_OVERRIDES_MARKET` when an AUTO crypto has both. The frontend shows it (frontend F34).
- The refresh job must cover every AUTO crypto instrument in use across all spaces in **one** CMC call per batch (`symbol=BTC,ETH,...` is supported, up to the plan limit), not one call per holding. **Verify** `marketdata/jobs` batching and the API credit use at the current refresh rate. Log the credits used per run (no key in the log).
- Staleness: crypto `StaleAfter` is a few refresh periods (`strategy.go`). A stale price still counts in the total (better than the typed price) and carries `stale: true` plus a `PRICE_STALE` warning, so the frontend can show it. A holding with no price at all keeps `Value` on the manual price and `priceSource = MANUAL`.
- Totals: `Summarize` (`valuation.go:177`), the grouped endpoint, `GET /assets/summary`, `net-worth/summary` and the daily snapshot job must all call the same `Value` with the same market price map, so the Home number, the Assets number and the graph agree. Add one test that asserts the four totals are equal for the same space and price.
- Endpoint for the client: `GET /spaces/{spaceId}/investments/{id}/price` returns `{ price, currency, source, asOf, stale }` (reuse the valuation fields) so the detail screen can show "Priced by CoinMarketCap, 2 min ago" without a full list refetch. Optional, can be dropped if the list DTO already carries `priceSource` and `priceAsOf` (it does: `valuation.go:43-44`).

### Tests (Phase G)
Create crypto without `priceMode` links and sets AUTO; ambiguous symbol falls back to MANUAL with the warning; explicit MANUAL stays MANUAL; the total changes when the fake CMC server returns a new price (the e2e harness already has `cmc_helpers_test.go`); stale price is used and flagged; manual value override warning; the four totals match; backfill migration on a seeded database; one batched CMC call for N holdings (assert request count on the fake server).

Acceptance: with the fake CMC client, a space with two crypto holdings shows a portfolio value equal to `Σ quantity × CMC price` converted to the base currency, to the cent. `make lint test test-e2e`.

## 3. Phase H: Currency model and investment defaults (T4, T8)

### 3.1 Native currency plus FX service (T4)
Decision D-B5 (proposed, recommended): keep what already exists (each investment in its native `quote_currency`, valued into the base currency through `currency.ConversionContext`) and make the FX rate **automatic by default**.

- `manualFxRate` becomes an optional override. When it is absent the valuation uses the stored FX rate (freecurrencyapi through `internal/currency`, daily job). Add a test: a USD stock in an IDR space with no `manualFxRate` is valued with the stored USD/IDR rate and no warning.
- `InvestmentDTO` gets `quoteCurrency` (it may exist already, **verify**), `fxRate` (string, the rate used), `fxSource` (`PROVIDER`, `MANUAL`) and `fxAsOf`. The forms no longer need to ask for a rate.
- `ID_STOCK` is IDR-only (`types.go:105`): the DTO exposes `quoteCurrency = "IDR"` so the form prefixes `Rp` on average price. The "USD/IDR fields" on that form are a frontend problem (frontend F35).
- If the provider has no rate for a pair (a stale or missing day), the existing `warnings[]` rule applies (item excluded from the sum and reported). Add `FX_RATE_MISSING` if the code differs today (**verify** the code name).
- `GET /fx/latest` stays the source for any UI that wants to show the rate. Add `?pairs=USD:IDR,EUR:IDR` if it does not take pairs yet (**verify**).
- Rate limit and quota: the free plan is small, so the job fetches `base_currency=USD` once a day for the union of currencies in use and derives cross rates. **Verify** this is what `currency/job.go` does and log the quota left.

### 3.2 No silent assumed return (T8)
Decision D-B6 (proposed, recommended): `estimated_annual_change_pct` becomes **nullable with no default**.

- Migration `00020_estimated_change_nullable.sql`: `ALTER COLUMN ... DROP NOT NULL, DROP DEFAULT`. Existing rows: keep their value (the user may have kept 5.5 knowingly) but set rows whose value is exactly `5.5` and whose `updated_at = created_at` to NULL, because those were never touched (**verify** the count first and ask in Q6 if it is large).
- **Verify** where the field is read: if a projection or goal calculation uses it, treat NULL as "no projection" (0%), never as 5.5. Add a test for each reader.
- `inputs.go`: absent means NULL. The DTO returns `null`.

### 3.3 Explicit "which value wins" (T8)
The rule already exists in `valuation.go:104-106` (market price if AUTO, else manual price; the manual current value wins over `quantity × price`). Expose it so the form can state it: `InvestmentDTO.valuedBy` (`MARKET_PRICE`, `MANUAL_PRICE`, `MANUAL_VALUE`), a rename of what `priceSource` already says. Keep `priceSource` and add no second field if it is equal (decision for the implementer; document it in swagger).

### Tests (Phase H)
FX default and override; DTO currency and rate fields; missing-rate warning; migration for the nullable change (touched row keeps 5.5, untouched becomes NULL); create without the field stores NULL; readers treat NULL as 0.

## 4. Phase I: Small API gaps (T7, T14)

- **Wallet delete impact (T7).** `GET /spaces/{spaceId}/wallets/{id}/delete-impact` returns `{ "transactions": 12, "transfers": 3, "goalReserves": 1, "balance": "134.26", "currency": "USD" }`. Counts only live rows. `DELETE` is unchanged but now requires `?confirm=true` **or** an `X-Confirm-Delete` header when the wallet has at least one transaction, else `409 WALLET_HAS_TRANSACTIONS` with the same counts. A wallet with no transactions deletes as today. (Decision D-B7: the server-side guard is optional if the team only wants the client confirmation. Recommended: add it, it is a few lines and stops a scripted delete.)
- **Multi-select categories (T14).** If `GET /transactions` takes only one `categoryId`, add `categoryIds` (repeated or comma list, max 50, UUID check, `400 VALIDATION_FAILED` otherwise) with OR semantics, combined with `types` by AND. Update the stats endpoints that take a category filter in the same way if they exist (**verify**). `categoryId` stays as an alias.
- **Filter result count.** Add `total` to the transactions list envelope if it only has a cursor (the filter badge shows a count). **Verify**, skip if present.

Tests: impact counts, soft-deleted rows not counted, guard returns 409 then 204 with confirm; `categoryIds` OR/AND semantics and invalid id.

## 5. Phase J: Wrapper rows for every asset (L2 support)

Frontend change only in the UI. One backend check: `GET /investments/grouped` must return a `wrappers[]` entry for a wrapper with **one** item (it does by design, `count: 1`), and `GET /assets/summary` must do the same for the assets page. Add a test with exactly one holding per wrapper and one per category, plus a null wrapper (`"Unassigned"`) with one item. No code change is expected. If the test fails, fix `grouping.go`.

## 6. Decisions

| ID | Decision | Status | Recommended |
| --- | --- | --- | --- |
| D-B1 | Top up becomes `ADJUSTMENT IN` | Proposed | Yes (section 1.1). Old rows are migrated |
| D-B2 | Explicit reclassify of an uncategorised income or expense to an adjustment | Proposed | Yes, no title guessing (section 1.2) |
| D-B3 | Localized seed names | Proposed | `system_key` now, localized seed names only when a language setting exists on the user or space. **Needs your answer** if you want seeds created in Indonesian |
| D-B4 | Crypto is AUTO by default when the symbol resolves | Proposed | Yes (section 2) |
| D-B5 | FX automatic, manual rate is an override | Proposed | Yes (section 3.1) |
| D-B6 | `estimated_annual_change_pct` nullable, no default | Proposed | Yes (section 3.2) |
| D-B7 | Server guard on wallet delete | Proposed | Yes, small |

## 7. Tasks (phased)

**Progress** (ticked when the phase's branch passes lint/tests and is pushed with a PR):
- [x] Phase F: ledger correctness (top up, reclassify, calendar net, net worth contributions, system keys, seeds)
- [x] Phase G: crypto priced by CMC
- [x] Phase H: currency model and nullable estimated change (the 5.5% data migration part of H-T4 is blocked by Q6, see section 12)
- [x] Phase I: wallet delete impact, list total (the multi-category filter already existed, C3)
- [x] Phase J: single-item wrapper test
- [ ] Phase K: growth history series (investments and assets, rising or falling per timeframe)

| Phase | Branch | Depends on | Size |
| --- | --- | --- | --- |
| F | `feat/ledger-correctness` | none | L |
| G | `feat/crypto-cmc-pricing` | none (Q5 for the backfill) | M |
| H | `feat/fx-and-defaults` | none | M |
| I | `feat/wallet-delete-impact` | none | S |
| J | `feat/wrapper-single-item-test` | Phase B (done) | S |

Order: F first (it fixes the numbers the user sees), then G, then H, then I and J.

**Phase F tasks (all done, Nov 23, 2026).** F-T1 failing test for top-up as income; F-T2 `TopUp` as adjustment; F-T3 migration 00017 and its test; F-T4 reclassify via PATCH and error codes; F-T5 `Day.net`, explicit type switch, SQL filter check; F-T6 history contribution fields; F-T7 `systemKey`; F-T8 `system_key` column, seed icons, alias conflict; F-T9 swagger and Appendix B.

**Phase G tasks (all done, Nov 23, 2026).** G-T1 prove the cause with a test (default create is MANUAL); G-T2 auto-link on create; G-T3 fallback and warnings; G-T4 batched refresh check and credit logging; G-T5 stale handling; G-T6 one valuation path for summary, grouped, assets summary, net worth and snapshots, with the equality test; G-T7 backfill migration (after Q5); G-T8 swagger.

**Phase H tasks (done except the Q6 data migration, Nov 23, 2026).** H-T1 FX default test; H-T2 DTO fields; H-T3 quota and cross-rate check in the job; H-T4 nullable migration; H-T5 readers audit; H-T6 `valuedBy` docs.

**Phase I tasks (done, Nov 23, 2026; I-T3 dropped by C3).** I-T1 delete-impact endpoint; I-T2 delete guard; I-T3 `categoryIds`; I-T4 list `total` if missing.

**Phase J tasks (done, Nov 23, 2026).** J-T1 test one holding per wrapper and category; fix only if it fails.

## 8. Risks

| ID | Type | Risk | Mitigation |
| --- | --- | --- | --- |
| RB-1 | Risk | Changing top up to an adjustment changes what existing clients see in history ("INCOME" becomes "ADJUSTMENT") | The shape is the same. The frontend plan F33 renders adjustments with the right label and sign. Release backend and frontend together |
| RB-2 | Risk | The migration reclassifies a real income that happens to be titled "Balance top up" | The title is generated by the backend and the row has no category. Print the count first, keep a down migration, ask in Q7 if the count looks wrong |
| RB-3 | Risk | CMC credits run out when more crypto is auto-priced | One batched call per refresh, credit logging, a configurable `CMC_REFRESH_MINUTES`, and a stale price still counts. The key stays in `.env` |
| RB-4 | Risk | The total jumps when old manual crypto rows flip to AUTO | Backfill only after Q5, only rows with an instrument and no manual value, and the log lists each change |
| RB-5 | Risk | `performancePct` is misread or wrong with large transfers in and out | Null when the denominator is not positive, tested with a top up and a price move, and the frontend labels it as return excluding deposits |
| RB-6 | Risk | Free FX plan quota is hit | One USD-based call per day, cross rates derived, quota logged |

## 9. Open questions

- Q5: Flip existing crypto holdings that have a CMC instrument to AUTO (so the total moves to live prices today), or only default new ones? The plan assumes yes for rows without a manual current value.
- Q6: Existing rows with exactly 5.5% and an untouched `updated_at` become NULL. Is that acceptable, or should every existing row keep its value?
- Q7: Should "Saldo awal" and "Posisi awal" typed by hand be reclassified automatically by title for this space, or only through the explicit action (D-B2)? The plan assumes the explicit action only.
- Q8: Is there, or will there be, a language setting (user or space)? It decides D-B3 and the localized seed names.
- Q9: For the portfolio total, should an AUTO crypto with a stale price count at the stale CMC price (planned) or fall back to the typed price?

## 10. Corrections from the dry run (Nov 23, 2026)

A read-only dry run of `plan/prompt.md` against the repo found the facts below. They override the text above where they differ. The sections above are left as written on purpose.

| # | Correction | Effect |
| --- | --- | --- |
| C1 | `development` is **not** at Phases A to E. Only A is merged. `feat/be-b-wrapper-grouping`, `be-c`, `be-d`, `be-e` and `feat/be-networth-investments-series` are unmerged (1 to 5 commits ahead). The highest migration on `development` is **00013**; 00014 to 00016 exist only on those branches | Migration numbers in this plan (00017 to 00020) are **examples**. Use `highest in internal/db/migrations at branch time + 1`, and re-number after a rebase. Phase J needs `grouping.go`, which only exists on `be-b`. See Q10 |
| C2 | `DailySummaryRows` (`internal/db/queries/transactions.sql:83-108`) has **no** `type IN ('INCOME','EXPENSE')` filter. `OverviewRows` and `FirstTransactionAt` do | Section 1.3 is confirmed as a real defect. Add the filter, keep the explicit `switch` |
| C3 | `categoryIds` already exists on `GET /transactions`, the stats and the daily endpoints (`transactions/handlers.go:299, 344, 546`) | Drop the `categoryIds` part of Phase I (I-T3). Keep the `total` field (I-T4), the list has only `nextCursor` |
| C4 | Wallet `DELETE` already has `?force=` and returns `422 WALLET_HAS_BALANCE` (`wallets/handlers.go:393-416`) | The planned `?confirm=true` / `X-Confirm-Delete` would collide. Fold the transaction-count guard into the existing `force` rule (force is required when the balance is not zero **or** the wallet has transactions) and add `delete-impact` as planned |
| C5 | The CMC client calls `/v2/simple/price` with `id=`, not `symbol=` (`coinmarketcap/client.go` about line 208), and `marketdata.go` already batches 100 per call | G-T4 is reduced to an assertion on the request count plus credit logging. No batching rewrite |
| C6 | `StaleAfter` lives in `internal/investments/valuation.go:17-27` (default 30 minutes, overridable per provider), with stock windows in `marketdata/strategy.go` | Reference fix only |
| C7 | The FX error code is `FX_RATE_UNAVAILABLE` (`currency/convert.go:102`), not `FX_RATE_MISSING`. `GET /fx/latest` takes `?base=`, not `?pairs=` | Reuse the existing code. Skip `?pairs=` unless the frontend needs it |
| C8 | `InvestmentDTO.quoteCurrency` already exists (`investments/handlers.go:83`). Only `fxRate`, `fxSource`, `fxAsOf` are new. `currency/service.go` `Refresh` already does one provider call and stores USD-quoted rates, cross rates are derived at convert time | H-T2 and H-T3 shrink to the three fields and a quota log line |
| C9 | `estimated_annual_change_pct` is read in `inputs.go:372,381,408`, `types.go:53,83,143` (also exposed as `Defaults.EstimatedAnnualChangePct = "5.5"` in the type metadata), `handlers.go:90,147`, `service.go:214,336,528` and the `investments` and `manual_assets` queries. `manual_assets` (00011) already allows NULL | H-T5 audits all of these, and the type-metadata default must become null too |
| C10 | `systemKey` is "derived from type and stored title". After D-B1 a top up and a manual balance change are both `ADJUSTMENT`, so type alone cannot separate `BALANCE_TOP_UP` from `BALANCE_ADJUSTMENT` | Derive by `type` plus the exact stored title (`Balance top up`, `Balance adjustment`, `Opening balance`). Store the key at write time for new rows instead (a nullable `system_key` on `transactions`, migration in Phase F) and backfill by title |

New open question: **Q10** Which branch is the base? Either merge `feat/be-b` to `feat/be-e` and `feat/be-networth-investments-series` into `development` first (recommended, the plan assumes they are in), or branch Phase F to I from the tip of `feat/be-networth-investments-series`. The prompt stops and asks until this is answered.
Q9 default (until answered): a stale CMC price still counts in the total and is flagged.


## 11. Resolutions and additions (Nov 23, 2026, second pass)

**Resolved.**
- **Q10:** the base is `development`. PR #24 merged the stacked Phases B to E and the networth series. Migrations now go up to 00016, so Phase F starts at 00017.
- **Q5 (yes, with a limit):** flip existing crypto holdings that have a CMC instrument to `AUTO`, so the **value** uses the current CMC price. The bought price is not touched: `avg_buy_price`, `fees` and `manual_price` are never overwritten, and cost basis stays `quantity x avg_buy_price + fees` (it is only used for profit and loss). G-T7 is no longer blocked. The migration must not write to any price or cost column, only `price_mode`, and a test asserts those columns are unchanged before and after.

**Phase K: growth history series.** The frontend needs to see, for a chosen timeframe, whether the **investments** and the **assets** (and the net worth) are rising or falling. Today `GET /spaces/{spaceId}/net-worth/history` returns per point only `netWorth` and `investments`, plus `changePct` and `investmentsChangePct`. The snapshot table already stores `cash`, `fixed_assets`, `receivables`, `liabilities` (migration 00013), but they are not in the API. All fields below are **additive** (non-breaking):

| Field | Where | Meaning |
| --- | --- | --- |
| `points[].cash`, `fixedAssets`, `receivables`, `liabilities` | each point | The other components of the net worth for that day, base currency, 2dp (IDR and JPY 0dp), taken from the snapshot. Live point computed now |
| `points[].assets` | each point | Convenience: `cash + investments + fixedAssets + receivables` (everything owned, debt not subtracted), so the "assets" line needs no client sum |
| `series` | top level | Per series (`netWorth`, `investments`, `fixedAssets`, `assets`): `{ first, last, change, changePct, performancePct, direction }`. `change` is `last - first` in money. `changePct` is first to last of the value. `performancePct` is the return **excluding money put in** (see below). `direction` is `RISING`, `FALLING` or `FLAT` from `change` (FLAT when the absolute `changePct` is below 0.05), so the client never decides it |
| `coverage` | top level | `{ firstPointDate, requestedFrom, short: bool }` so the frontend can say "your data only covers 3 months" (D-F43) without guessing |
| `range` | query | Keep `1M, 3M, 6M, 1Y, ALL`. Add `1W` and `YTD`. Max about 120 points, evenly downsampled, first and last kept (as now). One live point for today |

**Performance excluding deposits (needed so a rising line is not just money added).** For the investments series, a day's value rises when the user buys more, not only when prices rise. The chart must show both:
- `netContributions` (per series, money): for investments the sum of BUY minus SELL (and fees as costs) between the first and last point, from the trade records; for fixed assets the sum of purchases minus disposals; for net worth the adjustments, top ups and opening balances (Phase F, section 1.4).
- `marketChange = (last - first) - netContributions`, and `performancePct = marketChange / (first + netContributions)`; null when the denominator is not positive.
- Contributions are converted to the base currency with the rate of the trade date (the same `ConversionContext` rule as the dashboard). A missing rate adds a `FX_RATE_UNAVAILABLE` warning and leaves that trade out of the sum, never guesses.

**Known limit (say it in swagger and in the UI).** History only exists from the day the daily snapshot job first ran for a space (00:15 UTC, one row per day). There are no historical market prices stored, so earlier days cannot be rebuilt from trades. `coverage` reports this honestly. Do not backfill with invented values. An optional later step is a one-off backfill of days where every price is known (not part of this phase).

**Accuracy rules.**
- Same valuation path as Phase G (CMC price for AUTO crypto), so the last point equals the Home and Assets totals to the cent. Test it.
- The snapshot job runs after the CMC refresh, not before (check the cron order: 00:15 UTC snapshot vs a 10 minute refresh, which is fine, but assert the snapshot uses market prices and not manual ones).
- The live point and the stored points use the same currency (the space base currency at read time). Existing rule: history is returned in the current base currency.

**Tasks.** K-T1 failing test for a series that only has `investments` today; K-T2 expose the snapshot components and `assets` on points; K-T3 `series` block with `direction`; K-T4 contributions for investments (trades) and fixed assets (purchases), `performancePct`; K-T5 `coverage`, `1W` and `YTD` ranges; K-T6 equality test with the Home and Assets totals; K-T7 swagger (with the known limit) and Appendix B if a code is added. Branch `feat/growth-history-series`, depends on Phase F (the contribution maths for net worth) and G (prices). Size M.

New open question **Q11**: for `FLAT`, is a 0.05% band right, or should it be a setting? Planned default: 0.05%.


## 12. Implementation notes (dated)

**Nov 23, 2026, Phase F (`feat/ledger-correctness`).** F-T1 to F-T9 done.
- Test harness: before Phase F, 7 e2e tests failed on `development` because the real date had moved past the fixed Nov 2026 dates the tests use (FX rates were dated "today", the test clock was moved back to 18 Nov). Fixed in the harness only (`FakeFX.Clock`, `testClock.onSet`), no product code changed. `make test-e2e` was green again before any Phase F change.
- F-T5: `Day.Net` and `CalendarDayDTO.net` already existed, and `dayRows` already passes `Types = [INCOME, EXPENSE]`, so the calendar and analytics were never polluted by other types. Added the explicit `switch ... default: continue` and a test with every type. `DailySummaryRows` (the transactions daily-summary endpoint) was left without a type filter on purpose: it reports `transfer` totals separately.
- F-T4: no audit table exists in the schema, so the reclassification has no audit row; it is one statement in one database transaction. Existing behaviour changed on purpose per section 1.2: a PATCH `type` of `TRANSFER` or `OPENING_BALANCE`, or any type on an ADJUSTMENT/OPENING_BALANCE, is now `422 TYPE_CHANGE_NOT_ALLOWED` (it was `400`). Two existing assertions were updated.
- F-T3 / C10: `transactions.system_key` (migration 00017) is stored at write time (top up, opening balance, balance adjustment) and backfilled by exact title. The down migration turns every `BALANCE_TOP_UP` adjustment back into an INCOME, including new ones. Dev database copy: 4 top-up rows moved, wallet balance sum unchanged, up/down/up verified.
- F-T6: contributions are ADJUSTMENT and OPENING_BALANCE rows with `occurred_at` from the first point's day at 00:15 UTC (when the snapshot runs) to now, converted with the FX rate of the transaction's own date (`BuildContextAt`). A flow on the first point's day after 00:15 UTC counts as a contribution. Missing rate: left out with an `FX_RATE_UNAVAILABLE` warning (`warnings[]` is new on the history response). Phase K reuses this.
- F-T8: migration 00018 adds `categories.system_key`, a unique `(space_id, system_key)` index, and new icons. The seed `Dividend` shared the chart icon with `Investment`, so it was changed too (to a bank), and existing untouched rows are updated (only where the icon is still the old one). `Subscriptions` is now a repeat symbol. A seed-icon uniqueness unit test was added. D-B3: seeds stay in English (blocked by Q8), only the key was added. `CATEGORY_EXISTS_SIMILAR` is checked on create and on rename/type change, against live categories of the same type that are in the same alias group or carry the same key. An exact name is still `CATEGORY_NAME_TAKEN`.

**Nov 23, 2026, Phase G (`feat/crypto-cmc-pricing`, stacked on `feat/ledger-correctness` because of the migration numbers: this is 00019).** G-T1 to G-T8 done.
- G-T1: the cause was confirmed by a failing test (a crypto created without `priceMode` was MANUAL, unlinked, valued at the typed price).
- G-T2/T3 (D-B4): crypto defaults to AUTO when `priceMode` is absent (`Spec.AutoDefault`); the type metadata `defaults.priceMode` is now `AUTO` for CRYPTO only. If the coin is not found, or is ambiguous, the holding is created as MANUAL with a `PRICE_NOT_LINKED` item in `valuation.warnings`. An explicit `priceMode` never falls back (an explicit AUTO with an unknown symbol is still `422 INSTRUMENT_NOT_FOUND`). "Ambiguous" was not defined in the plan, so it is defined here: several active coins share the symbol and the best-ranked one has no rank or ranks below 500 (`marketdata.SymbolAmbiguous`). BTC (rank 1, with a clone at rank 3000) is not ambiguous. If you want another rule, change `ambiguousRankLimit`.
- Warnings: `PRICE_STALE` (a stale market price still counts, Q9 default) and `MANUAL_VALUE_OVERRIDES_MARKET` are added by `Value`. The portfolio `summary.warnings` still carries only `FX_RATE_UNAVAILABLE`.
- G-T4 / C5: batching already existed (100 coins per call). Added the credit log line (`coins`, `calls`, `quotes`, credits ~ calls) and an assertion that N holdings cost one call.
- G-T6: the list summary, the grouped view, the assets summary, the net-worth summary, the live history point and the snapshot job already shared one valuation path. No code change, one equality test added.
- G-T7 (Q5 yes): migration 00019 flips only `price_mode` for live CRYPTO rows with an instrument and no manual current value; rows without an instrument are listed with `RAISE NOTICE`. The test asserts avg_buy_price, manual_price, fees, quantity and manual value are identical before and after. The down migration is a no-op (a flipped row cannot be told from one the user set to AUTO). On a copy of the dev database no row was flipped (4 MANUAL crypto without an instrument, 1 already AUTO).
- Not done on purpose: the optional `GET /investments/{id}/price` endpoint (the list DTO already carries `priceSource`, `priceAsOf`, `isStale`).
- Existing test helpers that need typed prices (`crypto()` in grouping tests) now send `priceMode: MANUAL`.

**Nov 23, 2026, Phase H (`feat/fx-and-defaults`, stacked on `feat/crypto-cmc-pricing`, migration 00020).** H-T1 to H-T6 done, except the data part of H-T4.
- H-T1/T2: the valuation already used the stored provider rate when `manualFxRate` is absent (the test passed before any change). New DTO fields `fxRate`, `fxSource` (`PROVIDER` or `MANUAL`) and `fxAsOf` (also on `valuation.base`); all null when the quote currency is the base currency or no rate exists. `quoteCurrency` already existed (C8). `manualFxRate` stays an override only while the space's automatic FX is off (existing behaviour and test: with auto FX on it is ignored). The plan text "optional override" was not widened to override auto FX, because that would change an existing, tested rule. Tell me if you want it to win in auto mode too.
- H-T3 / C8: `Refresh` already makes one provider call and derives cross rates at convert time. The job runs every 6 hours (about 120 calls a month against the 5,000 free quota). Added a log line with `providerCalls` and, for freecurrencyapi, the remaining and total monthly quota from its `/status` endpoint (best effort).
- H-T4 (D-B6): migration 00020 makes `estimated_annual_change_pct` nullable with no default. **Blocked by Q6 and not done:** turning untouched 5.5 rows into NULL. All existing rows keep their value (13 rows on the dev database, all exactly 5.5, which shows why Q6 matters: the data cannot tell a typed 5.5 from the old default). New rows store NULL when the field is absent. PATCH accepts `null` to clear it.
- H-T5 audit (C9): the field is only stored and echoed. No goal, projection, net-worth or snapshot code reads it (searched `internal/`), so there is no reader to change; `manual_assets` was already nullable. Changed: create and patch inputs, the DTO, the type metadata (`defaults.estimatedAnnualChangePct` is now always null), the two queries, and the shared-investment row copy. `DefaultEstimatedAnnualChangePct` was removed.
- H-T6 (decision for the implementer): `valuedBy` was not added. `valuation.priceSource` already has the same three values (`MARKET`, `MANUAL`, `MANUAL_VALUE`); the swagger comment now documents which value wins.

**Nov 23, 2026, Phase I (`feat/wallet-delete-impact`, stacked on `feat/fx-and-defaults` so the plan file edits do not conflict; no migration).** I-T1, I-T2, I-T4 done, I-T3 dropped (C3, `categoryIds` exists).
- I-T1: `GET /spaces/{spaceId}/wallets/{id}/delete-impact` returns `transactions` (every transaction row pointing at the wallet as source or destination, so it includes opening balances and transfers), `transfers` (own-wallet transfers among them, not trades or settlements), `goalReserves`, `balance` and `currency`. Transactions are hard-deleted rows in this schema, so "live rows only" is automatic.
- I-T2 (C4, D-B7): folded into the existing `force` rule, no new `?confirm=` or header. `DELETE` without `force=true` is `422 WALLET_HAS_BALANCE` when the balance is not zero (unchanged), else `409 WALLET_HAS_TRANSACTIONS` with the counts in `details` when the wallet has any transaction. A wallet with no history still deletes without `force`.
- I-T4: `total` added to the transactions list envelope (count of all rows matching the filters, independent of the cursor and page size). It was missing (only `nextCursor`).

**Nov 23, 2026, Phase J (`feat/wrapper-single-item-test`, stacked on `feat/wallet-delete-impact`).** J-T1 done: the new test (`test/e2e/wrapper_single_item_test.go`) passed on the first run, so `grouping.go` was not changed. Checked: one holding per wrapper and per category gives a `wrappers[]` entry with `count: 1` and its item (also with `items=false`), the single wrapper of a category has `pctOfCategory` 100.00, and a single unassigned holding is listed last as `_unassigned` / "Unassigned". `GET /assets/summary` has categories but no wrappers, so for it the test checks that every category with a single holding is listed.
