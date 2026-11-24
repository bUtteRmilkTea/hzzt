-- name: GetValuationByDate :one
SELECT * FROM investment_valuations WHERE investment_id = $1 AND as_of = $2;

-- name: UpsertValuation :one
INSERT INTO investment_valuations (investment_id, as_of, nav, quantity, cost_basis, value, note, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('note'), $7)
ON CONFLICT (investment_id, as_of) DO UPDATE SET
    nav = EXCLUDED.nav, quantity = EXCLUDED.quantity, cost_basis = EXCLUDED.cost_basis, value = EXCLUDED.value,
    note = EXCLUDED.note, created_by_id = EXCLUDED.created_by_id, created_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: ListValuations :many
SELECT * FROM investment_valuations WHERE investment_id = $1 ORDER BY as_of DESC, created_at DESC;

-- name: GetValuation :one
SELECT * FROM investment_valuations WHERE id = $1 AND investment_id = $2;

-- name: DeleteValuation :exec
DELETE FROM investment_valuations WHERE id = $1 AND investment_id = $2;

-- name: UpdateInvestmentNav :exec
-- Copies the newest valuation into the investment (manual price and NAV date); the manual current value is never touched.
UPDATE investments SET manual_price = $3, details = $4, updated_at = now() WHERE id = $1 AND space_id = $2;
