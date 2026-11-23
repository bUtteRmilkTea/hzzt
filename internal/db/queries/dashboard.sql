-- name: OverviewRows :many
-- Income and expense totals per currency (transfers, adjustments and opening balances are not either).
SELECT t.type, t.currency, sum(t.amount)::numeric(38,8) AS total, count(*)::int AS cnt
FROM transactions t
WHERE t.space_id = $1 AND t.type IN ('INCOME', 'EXPENSE')
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR t.occurred_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR t.occurred_at < sqlc.narg('to_ts'))
GROUP BY t.type, t.currency;

-- name: BreakdownRows :many
-- Totals per category (NULL = uncategorized) and currency for one transaction type.
SELECT t.category_id, c.name AS category_name, c.icon AS category_icon, c.color AS category_color,
       t.currency, sum(t.amount)::numeric(38,8) AS total, count(*)::int AS cnt
FROM transactions t
LEFT JOIN categories c ON c.id = t.category_id
WHERE t.space_id = $1 AND t.type = sqlc.arg('type')::text
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR t.occurred_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR t.occurred_at < sqlc.narg('to_ts'))
GROUP BY t.category_id, c.name, c.icon, c.color, t.currency;

-- name: FirstTransactionAt :one
SELECT min(occurred_at) FROM transactions WHERE space_id = $1 AND type IN ('INCOME', 'EXPENSE');

-- name: UpsertNetWorthSnapshot :exec
INSERT INTO net_worth_snapshots (space_id, date, currency, cash, investments, fixed_assets, receivables, liabilities, net_worth)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (space_id, date) DO UPDATE SET
    currency = EXCLUDED.currency, cash = EXCLUDED.cash, investments = EXCLUDED.investments,
    fixed_assets = EXCLUDED.fixed_assets, receivables = EXCLUDED.receivables, liabilities = EXCLUDED.liabilities,
    net_worth = EXCLUDED.net_worth, created_at = now();

-- name: ListNetWorthSnapshots :many
SELECT * FROM net_worth_snapshots
WHERE space_id = $1 AND (sqlc.narg('from_date')::date IS NULL OR date >= sqlc.narg('from_date')::date)
ORDER BY date;

-- name: ListActiveSpaceIDs :many
SELECT id FROM spaces WHERE deleted_at IS NULL ORDER BY created_at, id;

-- name: ListNetWorthContributions :many
-- Money put into or taken out of the wallets from outside (adjustments, top ups, opening balances).
-- Own-wallet transfers, trades and settlements are neutral for the net worth and are not listed.
SELECT id, type, direction, amount, currency, occurred_at
FROM transactions
WHERE space_id = $1 AND type IN ('ADJUSTMENT', 'OPENING_BALANCE')
  AND occurred_at >= $2 AND occurred_at <= $3
ORDER BY occurred_at;
