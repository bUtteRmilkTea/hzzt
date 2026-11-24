# Feedback round 3: backend plan (trade history and revert, mutual fund valuations, field removal, crypto refresh)

Nov 24, 2026 · @jo

Scope: backend only (Go, chi, sqlc, goose, Postgres). Nothing here is implemented yet. Paths are relative to `/home/jonathanr/code/hzzt`. API base is `/api/v1`. Error envelope is `httpx.ErrorEnvelope` with `apperror` codes (see `plan/plan.md` Appendix B). This follows `plan/feedback-be-2-plan.md` (Phases F to K, done). It does not replace it. The frontend counterpart is `../hzzt-fe/plan/feedback-fe-9-plan.md`.

**Verification limits.** The feedback is text only (no screenshots), so every root cause below is **inferred** from reading the code, and marked "verify" where a query or a row must be looked at first. Each phase starts with a failing test (or a SQL check against a dev database) that proves the cause before the fix is written.

**Secrets.** The CoinMarketCap, Massive and FreeCurrency keys live in `.env` only (`CMC_API_KEY`, `MASSIVE_API_KEY`, `FREECURRENCY_API_KEY`). They are never written into a plan, a test, a log line or a commit.

## 0. Summary of findings

The feedback is eleven rows, numbered R1 to R11 here in the order they were written. Only the ones that need backend work are listed. The rest are frontend only (see the frontend plan, section 1).

| # | Item | Finding |
|---|---|---|
| R3 | Pie must union the same crypto across exchanges, show all, scroll | Frontend only. The list already returns one row per holding with `symbol` and `wrapper`, and the pie is built on the client from those rows. No new field needed. Phase L-T6 only checks that `symbol` is always filled for crypto |
| R6 | PnL for mutual funds, updated each month | A mutual fund has no price feed (`SupportsAutoPrice: false`, `types.go:127`). Its value is `manual_price` (the "current NAV") or `manual_current_value`, one number that the user overwrites. Nothing remembers the earlier months, so there is no monthly PnL, only today's `unrealizedPl`. `navDate` exists as a free text-like detail (`inputs.go:82`) and is never used to value anything |
| R7 | Duplicate exchange/broker input | Backend cause: the metadata (`types.go:106-150`) lists `exchangeOrWallet`, `broker`, `platform` as `detailFields`, and the investment also has a first class `wrapper` (`inputs.go:180`). `DetailsWrapper` (`inputs.go:367`) derives the wrapper from those details when `wrapper` is empty. So the form shows two inputs for one fact. US stock and ETF also have `exchange` (the listing, for example NASDAQ), which the user reads as the same thing. See Phase M |
| R8 | Top 3 brokers on the Assets page with PnL and % | Frontend only today (`accountGroups` already sorts by value and has the share). The group PnL is summed on the client from base-currency figures, which the plan rules (frontend plan 5.2) allow only for display of server figures. Phase L-T7 decides if the server should send the group PnL (D-B10) |
| R9 | Crypto price refresh every 15 minutes | `CMC_REFRESH_MINUTES` defaults to 10 (`config.go:154`, `.env.example:26`) and is validated 1 to 1440. `investments.StaleAfter` is 30 minutes (`valuation.go:19`), so 15 is safe (two missed refreshes before "stale"). The frontend never polls, so the screen shows the price from when it was loaded (frontend F43) |
| R10 | Transaction history on the asset detail page, deletable, state reverts | Half exists. `DELETE /transactions/{id}` on a BUY or SELL calls `Service.DeleteTrade` (`trade.go:424`) and restores the position, but only when the position still equals the position that trade left (409 `POSITION_CHANGED_SINCE`, `errPositionChanged`). So only the **latest** trade can be deleted. There is also **no list of trades per investment**: `GET /transactions` has no `investmentIds` filter (`transactions/handlers.go:304-350`). Manual assets already have `DELETE .../settlements/{txnId}` and a value history, but a value-change row cannot be reverted. See Phases N and O |
| R11 | Remove "Expected yearly change" and "Staked quantity" from assets in FE and BE | `estimated_annual_change_pct` is a column on `investments` (`00008`, nullable since `00020`) and on `manual_assets` (`00011`), and is in every DTO, input, patch, sqlc row and swagger. `stakedQuantity` lives in `details` jsonb of a crypto, is validated in `inputs.go:493`, is subtracted from what can be sold in `trade.go:121-131` and `position.go:79`, and is in the metadata `detailFields` (`types.go:111`). All requests are decoded with `DisallowUnknownFields` (`httpx/decode.go:109`, `inputs.go:116`), so an old client that still sends either field gets a 422. See Phase P |
| R1, R2, R4, R5 | Categories grid, header titles, detail page blank space, inline text on Investments | Frontend only |

## 1. Phase L: Small checks and the 15 minute refresh (R3, R8, R9)

### 1.1 Refresh every 15 minutes (R9)
- Change the default in `config.go:154` from 10 to 15 and `.env.example:26` to `CMC_REFRESH_MINUTES=15`. The validation range stays 1 to 1440. `marketdata.DefaultRefreshMinutes` (`marketdata.go:52`) is changed to match, and the comment in `plan/plan.md:749` budget line is updated (15 minutes is 96 calls a day, about 2,900 a month for the batched call, below the 10 minute figure).
- Keep **one batched CMC call per refresh** (feedback-be-2 G-T4). **Verify** that the job reads the interval from `cfg.CMCRefreshMinutes` (`cmd/api/main.go:133`) and has no second hard coded 10.
- Staleness: crypto goes `PRICE_STALE` after `investments.StaleAfter` (30 minutes). With a 15 minute job one failed refresh is still fresh and two are stale. That is the intended margin, so it is **not** changed. If the owner sets `CMC_REFRESH_MINUTES` above 30 the price would always be stale: add a config check that fails at start-up when `CMC_REFRESH_MINUTES * 2 > StaleAfter minutes` is violated, or derive `StaleAfter` from the interval (D-B8, recommended: derive, `max(30 min, 2 x interval)`).
- Existing deployments keep their own `.env` value. The new default only applies when the variable is unset, so the plan notes it in the release notes.

### 1.2 Crypto symbol is always filled (R3)
The pie unions holdings of the same coin by symbol. **Verify** with a SQL check that every `CRYPTO` row has a non-null `symbol` (`SymbolRequired: true`, `types.go:107`, so it should) and that the same coin is not stored in two spellings (`btc` and `BTC`). If the create path does not upper-case the symbol, normalise it on write (`NormalizeSymbol`) and in a one-off migration. A test creates `btc` and `BTC` and expects one symbol.

### 1.3 Group PnL for the Assets page (R8, D-B10)
The Assets page lists the top 3 brokers and exchanges with their PnL and share. Today the client sums `valuation.base.unrealizedPl` per wrapper. That is a sum of server figures, in one currency (the base), so it is allowed, but it can drift from the server by rounding when many rows are summed. Decision D-B10 (proposed): add `unrealizedPl`, `unrealizedPlPct` and `costBasis` to each `wrappers[]` entry of `GET /spaces/{spaceId}/investments/grouped` (additive, computed with the same decimal code as the category totals in `grouping.go`). If the user prefers the smaller change, skip this and keep the client sum.

Tests (Phase L): the default is 15 when unset and an explicit value still wins; out of range still fails; the derived `StaleAfter` for 15 and for 60; symbol normalisation; grouped wrapper PnL equals the sum of its items and the category total equals the sum of its wrappers (to the cent).

## 2. Phase M: One exchange or broker field (R7)

Goal: the create and edit forms ask for the exchange or broker **once**.

- Make `wrapper` the only place a broker, exchange or platform is stored for grouping. `DetailsWrapper` stays as the **read-side fallback** for old rows, so no row moves group.
- Remove `exchangeOrWallet` (crypto), `broker` (ID stock) and `platform` (US stock, ETF) from `Metas[].DetailFields` (`types.go:106-150`). They stay accepted on write for one release and are mapped into `wrapper` when `wrapper` is empty (so an old client does not break), and are never returned in `details`. After that release the three keys are dropped from `inputs.go` and from stored `details` by migration.
- US stock and ETF keep `exchange` because it is the **listing** (NASDAQ, NYSE), a different fact that is required. Rename its label to "Listing exchange" so it is no longer confused with the broker. The `GET /investment-types` response changes only in labels and the removed field entries. `exchangeOrWallet` for crypto has a second meaning (a self-custody wallet), which is covered by `wrapper` (any text up to 60 characters).
- Migration `00022_details_wrapper_fold.sql` (data): for rows with an empty `wrapper`, copy the first non-empty of `details.exchangeOrWallet`, `details.platform`, `details.broker` into `wrapper` (normalised with `NormalizeWrapper`), then remove those three keys from `details`. The migration prints the row count and is reversible only for the key move, not for rows that had two different values (the first non-empty wins, the rest is written to the migration log).
- Wrapper allocations (targets) key on `WrapperKey` of the display name, so nothing changes there. **Verify** with a test that a row migrated from `details.broker = "Stockbit"` and a new row with `wrapper = "stockbit "` land in one group and one target.

Tests (Phase M): metadata no longer lists the three fields; create with only `details.broker` still ends up in the group of the same `wrapper` (compat); migration moves the value and keeps the group; `exchange` stays required for US stock.

## 3. Phase N: Trade history per investment, deletable with revert (R10, investments)

### Current state
- `DeleteTrade` (`trade.go:424`) reverses the wallet through `ledger.Reverse`, deletes the row and writes `position_before` back. It needs `PositionOf(inv) == position_after` of that trade, so it is only possible for the newest trade.
- `UpdateTrade` has the same rule.
- Trades are `transactions` rows with `investment_id`, `investment_action` (BUY or SELL), `quantity`, `unit_price`, `fee`, `realized_pl`, `position_before`, `position_after` (`transactions.sql:6`).
- The initial position of a funded create is a BUY titled "Initial position {symbol}" (`service.go:387`). An investment created **without** funding has a starting quantity and average price that **no trade row explains**. Deleting trades can never go below that base, which the history must show.

### 3.1 List the trades of one investment
`GET /spaces/{spaceId}/investments/{id}/trades?cursor=&limit=` returns, newest first (`occurredAt desc, id desc`), cursor paged:

```
{ "items": [ { "id", "action": "BUY|SELL", "occurredAt", "quantity", "unitPrice", "fee", "amount", "currency",
               "walletId", "walletName", "realizedPl",
               "positionBefore": {quantity, avgBuyPrice, fees}, "positionAfter": {...},
               "canDelete": true, "blockedReason": null } ],
  "next": null, "openingPosition": { "quantity", "avgBuyPrice" } }
```

- Same viewer rules as `GET /investments/{id}` (a shared-in viewer may read, only the owner can delete: `OWNER_ONLY`, `requireOwner`).
- `openingPosition` is the position before the first trade (`position_before` of the oldest trade, or the whole current position when there are no trades), so the screen can show where the history starts.
- New sqlc query `ListTradesByInvestment`. It reuses the join in `transactions.sql:20-32` so the wallet name comes with it. Also add `investmentIds` to `GET /transactions` (repeated, same parsing as `walletIds`, `transactions/handlers.go:350`) so History can filter by asset later. Optional, listed so the filter does not need a third round.
- Index: `CREATE INDEX transactions_investment_idx ON transactions (investment_id, occurred_at DESC, id DESC) WHERE investment_id IS NOT NULL` (migration `00021`). **Verify** with `EXPLAIN` that the list uses it.

### 3.2 Delete any trade, not only the latest (D-B9)
Today the delete of an older trade answers 409 `POSITION_CHANGED_SINCE`. The feedback asks that the asset goes back to how it was before **that** transaction. For the latest trade that already works. For an older trade, "how it was before" means the position without it, with the later trades replayed on top. Decision D-B9:

| Option | What it does | Cost |
| --- | --- | --- |
| A. Latest only (today) | Only the newest trade shows a delete button. Older rows show "Delete the newer trades first" | None. Matches the 409 rule, but the user asked for delete on the transaction history, and a typo three trades ago stays stuck |
| B. Replay (recommended) | Delete the row, then recompute the position and every later trade's `position_before`, `position_after` and `realized_pl` from the opening position, in `occurred_at, id` order, in one database transaction with the investment row locked (`GetInvestmentForUpdate`). Wallet effects of the deleted trade are reversed with `ledger.Reverse`. Later trades keep their own wallet amounts (they were paid in that wallet at that price) | Medium. A SELL that came after a deleted BUY may no longer have enough quantity: the whole delete is refused with 422 `TRADE_DELETE_BREAKS_LATER_SELL` and a body that names the first trade that would go negative. Nothing is changed |

Replay rules (option B):
- Start from `openingPosition`. Apply each remaining trade with the existing `Position.Buy` and `Position.Sell` (`position.go`), so the maths is the one already tested. Because the staked quantity is removed (Phase P), `Sell` loses its `staked` argument.
- `realized_pl` of each later SELL is recomputed and written back, so the Realised P/L card and the portfolio totals stay equal to the history.
- For an ID stock, `lotsDetails` is recomputed with the new quantity (`trade.go:107`).
- The same function backs `UpdateTrade`, which then also stops needing the 409 rule. Keep that as a follow-up (not in this phase) so the scope stays on delete.
- Idempotent: a repeat delete of the same id answers 404 `TRANSACTION_NOT_FOUND`, never a second reversal.
- Audit: a row in `activity` if the project has one (**verify**, `grep -rn activity internal`), otherwise the log line has space id, user id, trade id and the position before and after. No amounts of other users.

### 3.3 Edge cases (all need a test)
- The **funding** BUY ("Initial position") can be deleted too: the position drops back to the opening position, the wallet is refunded. **Verify** that `fundInitialPosition` stores `position_before` as zero, so the opening position is zero and the investment can sit at quantity 0 (allowed by `CHECK (quantity >= 0)`).
- Shared investments: only the owner deletes, and the other participants' shares are percentages of the same position, so nothing per participant changes. Net worth snapshots of past days are **not** rewritten (they are a record of the day); only today's number moves. State it in the swagger description.
- A trade whose wallet was deleted: `ledger.Reverse` already handles a missing wallet? **Verify**. If not, delete still succeeds for the position and skips the wallet with a `warnings[]` entry.
- Concurrent trade and delete: both lock the investment row first (`GetInvestmentForUpdate`), then the wallet row, in that order, so there is no deadlock with the buy path (`lockInvestment`, `trade.go:210`).
- A manual current value (`manual_current_value`) wins over price in the valuation. A delete does not touch it, and the screen already warns about it (`MANUAL_VALUE_OVERRIDES_MARKET`).

### Tests (Phase N)
List order, paging, viewer rules, `openingPosition` with and without funding; delete the newest trade (same as today); delete a middle BUY and check quantity, average, fees, the later SELL's `realized_pl`, the wallet balance and the portfolio total all equal a fresh replay of the remaining trades; a delete that would make a later SELL negative answers 422 and changes nothing; a double delete is 404; shared-in viewer gets 403 `OWNER_ONLY`; deleting the funding BUY refunds the wallet. A property test: apply a random list of trades, delete a random one, and compare with building the same investment from the remaining list.

Acceptance: `make lint test test-e2e`, and the replay test passes with 200 random sequences.

## 4. Phase O: Manual asset history, revert a value change (R10, fixed assets, receivables, liabilities)

The Assets page also lists manual assets. Their detail page has a "History" of value changes and settlements. Settlements are already deletable (`DELETE /manual-assets/{id}/settlements/{txnId}`, `handlers.go:519`). A plain value update (`reason = UPDATED`) cannot be undone.

- Return, with `GET /spaces/{spaceId}/manual-assets/{id}`, the `id` of each `ValueChangeDTO` and `canRevert` (true for the **newest** entry of the asset and for settlements covered by the existing delete). Today the DTO has no id (`handlers.go:56`).
- `DELETE /spaces/{spaceId}/manual-assets/{id}/value-changes/{changeId}`: allowed when it is the newest change (otherwise 409 `VALUE_CHANGED_SINCE`). It sets `current_value` back to `previous_value`, deletes the row, and answers the asset. A settlement-reason row answers 409 `USE_SETTLEMENT_DELETE` with the settlement's transaction id, so the client calls the existing endpoint.
- Only the newest can be reverted because value changes are plain overwrites with no replay rule (a later change stores its own `previous_value`). Reverting an older one would need to rewrite the chain, which is not asked and is not safe. The frontend shows the delete only on the newest row.
- Deleting a settlement keeps today's behaviour (the wallet and the outstanding amount are reversed).

Tests (Phase O): revert the newest change restores the value; reverting an older one is 409; a settlement row gives the pointer error; a deleted asset answers 404; the net worth summary reflects the reverted value.

## 5. Phase Q: Mutual fund valuations, monthly PnL (R6)

### Design
A mutual fund has a quantity (units), an average buy price and a **current NAV** the user types. The user updates the NAV once a month. Keep each update as a dated **valuation** so the history of PnL exists, and keep today's number equal to the newest valuation.

New table, migration `00023_investment_valuations.sql`:

```
investment_valuations (
  id uuid PK, investment_id uuid NOT NULL REFERENCES investments(id) ON DELETE CASCADE,
  as_of date NOT NULL,                       -- the NAV date, not the typing date
  nav numeric(28,8) NOT NULL CHECK (nav >= 0),   -- price per unit on as_of
  quantity numeric(38,18) NOT NULL,          -- units held at that time (snapshot)
  cost_basis numeric(28,8) NOT NULL,         -- quantity x avg buy price + fees at that time
  value numeric(28,8) NOT NULL,              -- quantity x nav
  note text, created_by_id uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (investment_id, as_of)
)
```

- `POST /spaces/{spaceId}/investments/{id}/valuations` body `{ asOf: "2026-10-31", nav: "1,534.20" | "1534.20", note? }` (money strings as everywhere). Allowed for `MUTUAL_FUND` only (422 `VALUATIONS_NOT_SUPPORTED` otherwise, D-B11). Owner only. One per day (`UNIQUE`); a second POST for the same `asOf` replaces it (upsert, 200), so a typo is fixed by sending it again.
- It also copies the **newest** valuation into the investment (`manual_price = nav`, `details.navDate = asOf`), so the existing valuation code, totals, snapshots and the portfolio page keep working without a second path. A valuation older than the newest only adds history (no change to the live price). **Do not** touch `manual_current_value` (if the user typed one it still wins, and the warning already shows).
- The snapshot of `quantity` and `cost_basis` is taken at insert time from the position, so a later BUY does not rewrite last month's PnL.
- `GET .../valuations` returns newest first with, for each row, `pnl = value - costBasis`, `pnlPct` (null when cost is 0), and `changeFromPrevious` (value and percent against the previous row, with the position change excluded: `(nav - prevNav) x quantity` so a BUY or SELL does not show up as market gain). Server computed, decimal maths, base currency copy in `base` when a rate exists (same `currency.ConversionContext` as the list).
- `DELETE .../valuations/{valuationId}` removes one. If it was the newest, the investment's `manual_price` and `navDate` go back to the previous valuation (or stay as they are when it was the only one, and the response says so with a warning `NAV_NOT_REVERTED`). This is the same "revert" idea as Phases N and O.
- Missing months: the list does not invent rows. `GET .../valuations` also returns `lastValuationAt` and the investment DTO gets `valuation.navAgeDays` (null for non mutual funds) so the client can say "NAV is 41 days old". No scheduled job and no notification in this phase (see Q20).
- Existing mutual funds start with no history. Backfill: one row per fund from today's `manual_price` and `navDate` (or `updated_at` when empty) so the screen is not empty. Migration `00024`, listed in the migration log.

### Why a table and not only the price
The existing fields keep one number. A table keeps the dated numbers, allows a delete, and is the same shape the growth chart (Phase K) can read later: the per-fund value per month. The chart is **not** changed here.

### Tests (Phase Q)
Create a valuation updates the live price and `navDate`; an older date adds history only; the same date replaces; PnL and `changeFromPrevious` with a BUY between two months (the BUY must not count as gain); non fund type is 422; a viewer who is not the owner is 403; delete of the newest restores the previous NAV; totals (summary, grouped, assets summary, net worth) equal after each step; the backfill migration creates one row for an existing fund.

## 6. Phase P: Remove expected yearly change and staked quantity (R11)

The feedback says "from asset in fe and be". The plan removes both **everywhere they exist**, not only on investments, so that no screen keeps the field: investments (`estimated_annual_change_pct`, `stakedQuantity`) and manual assets (`estimated_annual_change_pct`, "Estimated change % a year" on a fixed asset). The owner confirmed (Nov 24, 2026) that it is removed everywhere.

### 6.1 Estimated annual change
- Migration `00025_drop_estimated_change.sql`: `ALTER TABLE investments DROP COLUMN estimated_annual_change_pct; ALTER TABLE manual_assets DROP COLUMN estimated_annual_change_pct;`. The Down migration re-adds both as nullable (`numeric(8,4)`, the `00020` shape), data is not restored. The migration prints how many rows had a value, for the release notes.
- Remove it from: `internal/investments/inputs.go` (lines 35, 186, 439, 464-468, 579, 600-602, 676-677), `handlers.go:94-95,177-179`, `types.go:92-93` (`TypeDefaults.EstimatedAnnualChangePct`), `service.go:216,348,554`, `internal/manualassets/handlers.go` (75, 91-93, 120, 149, 255-257, 311, 344-349, 390, 426-428) and `service.go` (`EstimatedChange`), `internal/db/queries/investments.sql` and `manual_assets.sql` (`UpdateManualAsset` set list). Then `make sqlc` and `make swag` regenerate `internal/db/sqlc/*` and `docs/`.
- Check that nothing reads it for a projection: `grep -rn "EstimatedAnnualChange\|estimated_annual" internal` after the change must be empty. **Verify** `networth` and `growth` do not use it.
- API shape: the field disappears from every response. Because requests use `DisallowUnknownFields`, an old frontend that still sends `estimatedAnnualChangePct` gets 422 `VALIDATION_FAILED`. To make the release safe, **accept and ignore** the field on POST and PATCH of both resources for one release (a tolerant `ignored` struct field with a `// Deprecated` swagger note and a log line), then delete it. D-B12 (recommended). The swagger drops it from responses at once.

### 6.2 Staked quantity
- Remove `stakedQuantity` from `cryptoDetails` (`inputs.go:65`), its validation (`inputs.go:493-497`), `Metas` crypto `DetailFields` (`types.go:111`), the example text in `FieldMeta` (`types.go:64-65`), `stakedOf` and `checkLeg` use (`trade.go:121-131`, `trade.go:176`), and `Position.Sell`'s `staked` parameter and message (`position.go:76-83`). Selling is then limited by `quantity` only, and the message becomes "Only {quantity} can be sold".
- Swagger text in `investments/handlers.go:306` and `transfers/handlers.go:222` ("CRYPTO checks `details.stakedQuantity` <= quantity", "Selling more than quantity − stakedQuantity answers 422 INSUFFICIENT_QUANTITY") is rewritten.
- Data: migration `00026_strip_staked_quantity.sql` removes the key (`details = details - 'stakedQuantity'`) because the typed `details` decoder rejects unknown keys when a row is read (`inputs.go:116`). **Verify** with `SELECT count(*) FROM investments WHERE details ? 'stakedQuantity'` and with how many have a value above zero. For those rows the sellable amount goes up from `quantity - staked` to `quantity`. The migration prints the list (space id, investment id, staked amount) for the record, the owner already accepted the change (Q22). The down migration cannot restore the amount, so the list is the backup.
- Accept and ignore `details.stakedQuantity` for one release on create and patch (same D-B12 rule), so an old client keeps working.

### 6.3 Order
P goes **before** N. Phase N rewrites the position replay on top of `Position.Sell`, and P-T4 removes the `staked` argument of that function, so doing P first means `Sell` is changed once.

### Tests (Phase P)
Create and patch without the fields; create and patch **with** the old fields still answer 2xx and store nothing; swagger has neither field; selling the whole quantity of a crypto that used to have a staked amount works; migration strips the key and the list API still decodes the row; `grep` check in CI (a small test that fails when the identifiers come back, kept for one release).

## 7. Decisions

| ID | Decision | Status | Recommended |
| --- | --- | --- | --- |
| D-B8 | Derive `StaleAfter` from the refresh interval instead of a fixed 30 minutes | Proposed | Yes: `max(30 min, 2 x interval)` (section 1.1) |
| D-B9 | Deleting an older trade | Proposed | Option B, replay, refuse with 422 when a later SELL would go negative (section 3.2) |
| D-B10 | Group PnL from the server on `wrappers[]` | Proposed | Yes, small and additive (section 1.3) |
| D-B11 | Valuations only for mutual funds | Proposed | Yes. Other manual-priced types can be added later without a schema change |
| D-B12 | Removed fields are accepted and ignored for one release | Proposed | Yes, then deleted. Avoids a 422 for a frontend that deploys a few minutes later |
| D-B13 | Manual asset revert only for the newest value change | Proposed | Yes (section 4) |

## 8. Tasks (phased)

**Progress** (ticked when the phase's branch passes lint/tests and is pushed with a PR):
- [x] Phase L: refresh 15 minutes, symbol check, grouped wrapper PnL
- [x] Phase P: remove expected yearly change and staked quantity
- [ ] Phase M: one exchange or broker field
- [ ] Phase N: trade history per investment, delete with replay
- [ ] Phase O: manual asset value change revert
- [ ] Phase Q: mutual fund valuations and monthly PnL

| Phase | Branch | Depends on | Size |
| --- | --- | --- | --- |
| L | `feat/crypto-refresh-15m` | none | S |
| P | `feat/drop-estimated-change-staked` | none | M |
| M | `feat/single-wrapper-field` | none | M |
| N | `feat/investment-trade-history` | P (the `Sell` signature) | L |
| O | `feat/manual-asset-revert` | none | S |
| Q | `feat/mutual-fund-valuations` | P (removes the column the fund form used for projections) | M |

Order: L first (smallest, one line of config), then P (it changes `Sell` and the DTOs the others touch), then M, N, O and Q in any order. Release backend and frontend together for P and M, because both change request shapes (D-B12 makes that safe but the screens must drop the inputs in the same release).

**Phase L tasks.** L-T1 failing test: default is 10; L-T2 default 15 in `config.go`, `.env.example`, `marketdata.DefaultRefreshMinutes`, `plan/plan.md` budget line; L-T3 derived `StaleAfter` and the start-up check (D-B8); L-T4 symbol normalisation and the SQL check; L-T5 grouped wrapper PnL (D-B10); L-T6 swagger.

**Phase P tasks.** P-T1 SQL counts (rows with an estimated change, rows with `stakedQuantity` above zero) printed in the PR; P-T2 failing tests for the old and new request shapes; P-T3 migration 00025 and the Go and sqlc removal for estimated change; P-T4 staked removal in `inputs`, `types`, `trade`, `position`; P-T5 tolerant `ignored` fields (D-B12); P-T6 migration 00026 strip key; P-T7 swagger and Appendix B; P-T8 CI grep test.

**Phase M tasks.** M-T1 failing test: metadata lists `broker`, `platform`, `exchangeOrWallet` next to `wrapper`; M-T2 remove the three from `Metas` and accept them on write mapped to `wrapper`; M-T3 rename the US `exchange` label; M-T4 migration 00022 fold; M-T5 group and target test; M-T6 swagger.

**Phase N tasks.** N-T1 index and `ListTradesByInvestment`; N-T2 `GET .../trades` and `investmentIds`; N-T3 `ReplayPositions` (pure function, table tests); N-T4 `DeleteTrade` through replay and the 422 rule; N-T5 funding BUY, deleted wallet and shared cases; N-T6 property test; N-T7 swagger and Appendix B (`TRADE_DELETE_BREAKS_LATER_SELL`).

**Phase O tasks.** O-T1 ids and `canRevert` in the DTO; O-T2 `DELETE .../value-changes/{changeId}`; O-T3 tests; O-T4 swagger.

**Phase Q tasks.** Q-T1 migration 00023 and sqlc; Q-T2 `POST` upsert and the live price copy; Q-T3 `GET` with PnL and `changeFromPrevious`; Q-T4 `DELETE` with the revert rule; Q-T5 `navAgeDays`; Q-T6 backfill migration 00024; Q-T7 equality test of all totals; Q-T8 swagger.

## 9. Risks

| ID | Type | Risk | Mitigation |
| --- | --- | --- | --- |
| RB-7 | Risk | Replay on delete rewrites later trades and the user loses a record they relied on (their realised PnL changes) | Refuse when a later SELL breaks, show the effect in the frontend confirm text, and log before and after. The old row values are written to the log line |
| RB-8 | Risk | Deleting the "Initial position" BUY leaves an investment at quantity 0 | It is a valid state. The frontend explains it and offers Delete investment |
| RB-9 | Risk | Dropping a column cannot be undone | The migration prints the counts, the Down re-adds the column (empty), and a database backup is taken first (the release step in section 10) |
| RB-10 | Risk | Selling more crypto than before for rows that had a staked amount | Q22 is answered (fully sellable). The migration prints the list for the record |
| RB-11 | Risk | Two clients with different shapes during the deploy | D-B12: the removed fields are accepted and ignored for one release |
| RB-12 | Risk | 15 minute refresh and a CMC outage make everything stale after 30 minutes | Same as before: a stale price still counts and is flagged (feedback-be-2 G-T5). D-B8 keeps the margin when the interval is raised |
| RB-13 | Risk | A valuation typed with the wrong date replaces the right one | The upsert is by date and the old value is in the log line. Delete and re-enter is possible |
| RB-14 | Risk | The mutual fund PnL is read as a return rate | The label is PnL since cost and, separately, change since the previous valuation. Neither is annualised |

## 10. Open questions

**Answered by the owner (Nov 24, 2026)**
- Q21 (answered): "Expected yearly change" is removed for **all** assets, investments and manual assets (Phase P-T3 as written).
- Q22 (answered): crypto rows that had a staked quantity become fully sellable. The staked amount is removed, not migrated anywhere (Phase P-T4 and P-T6 as written). The migration still prints the list in the PR for the record, but no approval step is needed before applying it.

**Still open**
- Q19: Should "delete an older trade" (D-B9, option B, replay) be done now, or only the newest? The plan assumes replay, with the 422 safety rule.
- Q20: Should the app remind the user to update a mutual fund's NAV each month (a notification or a banner)? The plan only shows the age of the NAV ("41 days old"). A reminder needs a job and a channel, which does not exist yet.
- Q23: Should a mutual fund update accept a **total value** ("my fund is worth 12,300,000 now") as well as a NAV per unit? The plan stores NAV, and the form can compute it from value and units (frontend F42), so the API stays one shape.

## 11. Implementation notes (dated)

(Add a dated line per phase when it ships.)

- Nov 24, 2026, Phase L done on `feat/crypto-refresh-15m` (L-T1 to L-T6 all done): default refresh 15 minutes (`config.go`, `.env.example`, `marketdata.DefaultRefreshMinutes`, `plan/plan.md` budget line); crypto staleness is derived as `max(30 min, 2 x interval)` (`marketdata.CryptoStaleAfter`, D-B8, no start-up check needed); symbol normalisation covered by a test only (already upper-cased); grouped wrapper PnL: `wrappers[]` already carried `cost`, `unrealizedPl` and `unrealizedPlPct` (embedded `TotalsDTO`, so D-B10 was already in place), a test now proves wrapper PnL equals the sum of its items and the category and total equal the sum of their wrappers; swagger regenerated with no change.

## 12. Corrections from the dry run (Nov 24, 2026)

Dry run on `development` at b7d39f1 (= origin/development, no unmerged branches). Baseline: `make lint` 0 issues, `go test ./...` and `make test-e2e` all green. sqlc and swag are installed and regenerate with no diff. The app starts against the dev database (liveness is `GET /api/v1/health`, readiness `/api/v1/health/ready`; there is no `/health` at the root). No finding changes a D-B* decision.

- Migration numbers: highest is 00020, so the real numbers are assigned at branch time (highest + 1 per branch). The numbers 00021 to 00026 in this plan are examples only.
- **Index name clash (1.3 / N-T1):** `transactions_investment_idx` already exists (migration 00007, on `(investment_id)` where not null). The new index must use another name, for example `transactions_investment_occurred_idx` on `(investment_id, occurred_at DESC, id DESC) WHERE investment_id IS NOT NULL`, and may drop the old one (it is a prefix of the new one). `EXPLAIN` (with seqscan disabled, the dev table is tiny) uses the new index for the list query and needs no sort step.
- **L-T4:** `cleanSymbol` (`inputs.go:312`) already trims and upper-cases, so symbol normalisation needs only a test. Dev data: 0 CRYPTO rows with a symbol that differs from its upper-case form, so no data migration.
- **R9 / L-T2:** the job interval comes from `cfg.CMCRefreshMinutes` (`cmd/api/main.go:133`), but `marketdata.DefaultRefreshMinutes = 10` (`marketdata.go:53`) is a second hard-coded 10 and must change too. The default is in `config.go:154`.
- **Q20 (no reminder) and Q23 (NAV only)** are answered: no reminder feature, the valuation API takes a NAV only.
- **N (activity):** there is no activity table. `grep` finds only the participant audit (`auditParticipant` in `shares.go`, share changes). The trade delete writes a structured log line instead.
- **N (verify list):** `fundInitialPosition` stores `position_before` as a zero `Position{}`, so the funding BUY has opening position zero. `ledger.Reverse` calls `AddToWalletBalance`, which filters only on id and space (not `deleted_at`), so reversing into a soft-deleted wallet works. It fails with an error only when the wallet row is gone, which the schema prevents for soft delete.
- **P (strip-key migration):** stored details are read leniently (`parseStoredDetails`, plain `json.Unmarshal`), and a PATCH rebuilds the typed details from the stored row (`specFromRow`), so unknown stored keys are dropped and never re-validated. The strip migration is tidy-up, not required for reads or patches. Only the request path (`decodeDetails`) is strict. Dev data: 0 rows with `stakedQuantity`; 13 investments and 0 manual assets have an estimated change value.
- **M (data):** dev data has 5 investments with one of `exchangeOrWallet`, `platform`, `broker` in details, 8 investments with an empty wrapper, and 1 mutual fund.
- **D-B10 already in place:** `WrapperGroupDTO` embeds `TotalsDTO` (`value`, `cost`, `unrealizedPl`, `unrealizedPlPct`) and `Group` computes them with decimal sums, so L-T5 is a test only. The field is named `cost`, not `costBasis`.

## 13. Phase P implementation note (Nov 24, 2026)

Phase P done on `feat/drop-estimated-change-staked` (P-T1 to P-T8 all done; branched from the Phase L branch only because that branch carries this plan file). The real migration numbers are 00021 (drop `estimated_annual_change_pct` on both tables, prints counts) and 00022 (strip `stakedQuantity`, prints the list). Dev data counts before the migration: 13 investments and 0 manual assets with an estimated change, 0 investments with `stakedQuantity`. Both migrations ran up, down, up on a scratch copy of the dev database. Old request fields are accepted and ignored (`estimatedAnnualChangePct` on investments and manual assets, `details.stakedQuantity` on crypto) with a warning log line, never stored or returned. `Position.Sell` lost its `staked` argument. A source scan test (`TestRemovedFieldsDoNotComeBack`) fails if the identifiers return outside the tolerant fields. Appendix B of `plan/plan.md` was adjusted.
