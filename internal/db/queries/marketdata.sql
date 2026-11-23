-- name: UpsertInstruments :exec
-- Bulk upsert of one provider's instruments from parallel arrays (set-returning functions zip together).
INSERT INTO market_instruments (provider, external_id, symbol, name, kind, currency, rank, active, updated_at)
SELECT sqlc.arg('provider')::text,
       unnest(sqlc.arg('external_ids')::text[]),
       unnest(sqlc.arg('symbols')::text[]),
       unnest(sqlc.arg('names')::text[]),
       sqlc.arg('kind')::text,
       sqlc.arg('currency')::text,
       NULLIF(unnest(sqlc.arg('ranks')::int[]), 0),
       unnest(sqlc.arg('actives')::bool[]),
       now()
ON CONFLICT (provider, external_id) DO UPDATE SET
    symbol = EXCLUDED.symbol, name = EXCLUDED.name, rank = EXCLUDED.rank,
    active = EXCLUDED.active, updated_at = now();

-- name: CountInstruments :one
SELECT count(*)::int FROM market_instruments WHERE provider = $1 AND kind = $2;

-- name: GetInstrument :one
SELECT * FROM market_instruments WHERE id = $1;

-- name: SearchInstruments :many
-- Ranked: exact symbol, then symbol prefix, then name matches; within a tier by market-cap rank.
SELECT * FROM market_instruments
WHERE provider = $1 AND kind = $2 AND active
  AND (upper(symbol) = upper(sqlc.arg('q')::text)
       OR upper(symbol) LIKE upper(sqlc.arg('prefix')::text) || '%'
       OR name ILIKE '%' || sqlc.arg('q_escaped')::text || '%')
ORDER BY (upper(symbol) = upper(sqlc.arg('q')::text)) DESC,
         (upper(symbol) LIKE upper(sqlc.arg('prefix')::text) || '%') DESC,
         rank ASC NULLS LAST, name
LIMIT sqlc.arg('row_limit')::int;

-- name: BestInstrumentBySymbol :one
SELECT * FROM market_instruments
WHERE provider = $1 AND kind = $2 AND active AND upper(symbol) = upper(sqlc.arg('symbol')::text)
ORDER BY rank ASC NULLS LAST, id
LIMIT 1;

-- name: SymbolMatches :one
-- How many active instruments share a symbol, and the best (lowest) market-cap rank among them.
SELECT count(*)::int AS n, COALESCE(min(rank), 0)::int AS best_rank
FROM market_instruments
WHERE provider = $1 AND kind = $2 AND active AND upper(symbol) = upper(sqlc.arg('symbol')::text);

-- name: UpsertMarketPrice :exec
INSERT INTO market_prices (instrument_id, price, currency, change_24h_pct, as_of, fetched_at)
VALUES ($1, $2, $3, sqlc.narg('change_24h_pct'), $4, now())
ON CONFLICT (instrument_id) DO UPDATE SET
    price = EXCLUDED.price, currency = EXCLUDED.currency, change_24h_pct = EXCLUDED.change_24h_pct,
    as_of = EXCLUDED.as_of, fetched_at = now();

-- name: GetMarketPrices :many
SELECT * FROM market_prices WHERE instrument_id = ANY($1::uuid[]);

-- name: HasMarketPrice :one
SELECT EXISTS (SELECT 1 FROM market_prices WHERE instrument_id = $1);

-- name: InsertJobRun :exec
INSERT INTO job_runs (name, started_at, finished_at, status, items_processed, error)
VALUES ($1, $2, $3, $4, $5, sqlc.narg('error'));

-- name: TryJobLock :one
-- Transaction-scoped advisory lock keyed by the job name.
SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0));

-- name: SetInvestmentInstrument :exec
UPDATE investments SET instrument_id = sqlc.narg('instrument_id'), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: AutoPricedByProvider :many
-- Distinct instruments of live AUTO investments for one provider (any kind).
SELECT DISTINCT i.id, i.external_id, i.symbol
FROM investments inv
JOIN market_instruments i ON i.id = inv.instrument_id
WHERE inv.deleted_at IS NULL AND inv.price_mode = 'AUTO' AND i.provider = $1 AND i.active
ORDER BY i.external_id;

-- name: GetMarketPricesWithProvider :many
SELECT p.instrument_id, p.price, p.as_of, i.provider
FROM market_prices p
JOIN market_instruments i ON i.id = p.instrument_id
WHERE p.instrument_id = ANY($1::uuid[]);
