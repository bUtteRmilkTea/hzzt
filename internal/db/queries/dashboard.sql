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

-- name: ListInvestmentTradeFlows :many
-- BUY and SELL trades of the space's investments in a window, in the investment's quote currency.
SELECT t.investment_action, t.quantity, t.unit_price, t.fee, t.occurred_at, i.quote_currency
FROM transactions t
JOIN investments i ON i.id = t.investment_id
WHERE t.space_id = $1 AND t.type = 'TRANSFER' AND t.investment_action IN ('BUY', 'SELL')
  AND t.occurred_at >= sqlc.arg('from_ts') AND t.occurred_at <= sqlc.arg('to_ts')
ORDER BY t.occurred_at;

-- name: ListUnfundedInvestmentCreations :many
-- Investments added in a window with no trade at all (no funding wallet): their cost counts as money put in.
SELECT i.quote_currency, i.quantity, i.avg_buy_price, i.fees, i.created_at
FROM investments i
WHERE i.space_id = $1 AND i.deleted_at IS NULL
  AND i.created_at >= sqlc.arg('from_ts') AND i.created_at <= sqlc.arg('to_ts')
  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.investment_id = i.id);

-- name: ListFixedAssetFlows :many
-- Fixed assets bought (first value row, from 0) and disposed of (deleted) in a window.
SELECT 'PURCHASE'::text AS flow, c.new_value AS value, a.currency, c.changed_at AS at
FROM manual_asset_value_changes c
JOIN manual_assets a ON a.id = c.asset_id
WHERE a.space_id = $1 AND a.kind = 'FIXED_ASSET' AND c.previous_value = 0 AND c.reason = 'MANUAL'
  AND c.changed_at >= sqlc.arg('from_ts') AND c.changed_at <= sqlc.arg('to_ts')
UNION ALL
SELECT 'DISPOSAL'::text, a.current_value, a.currency, a.deleted_at
FROM manual_assets a
WHERE a.space_id = $1 AND a.kind = 'FIXED_ASSET' AND a.deleted_at IS NOT NULL
  AND a.deleted_at >= sqlc.arg('from_ts') AND a.deleted_at <= sqlc.arg('to_ts')
ORDER BY 4;
