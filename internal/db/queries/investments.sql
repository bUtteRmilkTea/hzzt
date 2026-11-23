-- name: CreateInvestment :one
INSERT INTO investments (
    space_id, asset_type, price_mode, name, symbol, icon, quote_currency, quantity, avg_buy_price,
    manual_price, fees, manual_current_value, purchase_date, estimated_annual_change_pct,
    manual_fx_rate, details, created_by_id, instrument_id, wrapper
) VALUES (
    $1, $2, $3, $4, sqlc.narg('symbol'), $5, $6, $7, $8,
    $9, $10, sqlc.narg('manual_current_value'), sqlc.narg('purchase_date'), $11,
    sqlc.narg('manual_fx_rate'), $12, $13, sqlc.narg('instrument_id'), sqlc.narg('wrapper')
)
RETURNING *;

-- name: GetInvestment :one
SELECT * FROM investments WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: GetInvestmentForUpdate :one
SELECT * FROM investments WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL FOR UPDATE;

-- name: ListInvestments :many
SELECT * FROM investments
WHERE space_id = $1 AND deleted_at IS NULL
  AND (sqlc.narg('asset_type')::text IS NULL OR asset_type = sqlc.narg('asset_type')::text)
ORDER BY created_at, id;

-- name: UpdateInvestment :one
-- Full replacement of the editable columns: the service computes the final values.
UPDATE investments SET
    price_mode = $3, name = $4, symbol = sqlc.narg('symbol'), icon = $5, quote_currency = $6,
    quantity = $7, avg_buy_price = $8, manual_price = $9, fees = $10,
    manual_current_value = sqlc.narg('manual_current_value'), purchase_date = sqlc.narg('purchase_date'),
    estimated_annual_change_pct = $11, manual_fx_rate = sqlc.narg('manual_fx_rate'),
    details = $12, wrapper = sqlc.narg('wrapper'), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteInvestment :execrows
UPDATE investments SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: UpdateInvestmentPosition :one
UPDATE investments SET quantity = $3, avg_buy_price = $4, fees = $5, details = $6, updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: RealizedPLByInvestment :many
-- Realized profit/loss of every SELL, summed per investment (in each investment's quote currency).
SELECT t.investment_id, COALESCE(sum(t.realized_pl), 0)::numeric(28,8) AS realized_pl, count(*)::int AS sells
FROM transactions t
WHERE t.investment_id = ANY($1::uuid[]) AND t.investment_action = 'SELL'
GROUP BY t.investment_id;

-- name: GetInvestmentByID :one
-- Any live investment, regardless of space (for participant flows).
SELECT * FROM investments WHERE id = $1 AND deleted_at IS NULL;

-- name: GetInvestmentForViewer :one
-- An investment the viewer owns through this space, or that is shared with them and attached to this space.
SELECT i.* FROM investments i
WHERE i.id = $1 AND i.deleted_at IS NULL
  AND (
    i.space_id = $2
    OR EXISTS (
      SELECT 1 FROM investment_participants p
      WHERE p.investment_id = i.id AND p.status = 'ACTIVE' AND p.role = 'PARTICIPANT'
        AND p.user_id = $3 AND p.attached_space_id = $2
    )
  );

-- name: GetViewerParticipation :one
-- The viewer's live participant row of a shared investment, as seen from the space it is attached to.
SELECT * FROM investment_participants
WHERE investment_id = $1 AND user_id = $2 AND attached_space_id = $3
  AND status = 'ACTIVE' AND role = 'PARTICIPANT';
