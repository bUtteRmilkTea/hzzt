-- name: CreateGoal :one
INSERT INTO goals (space_id, name, icon, color, target_amount, currency, target_date, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('target_date'), $7)
RETURNING *;

-- name: GetGoal :one
SELECT * FROM goals WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: GetGoalForUpdate :one
SELECT * FROM goals WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL FOR UPDATE;

-- name: ListGoals :many
SELECT * FROM goals WHERE space_id = $1 AND deleted_at IS NULL ORDER BY created_at, id;

-- name: UpdateGoal :one
UPDATE goals SET
    name = $3, icon = $4, color = $5, target_amount = $6, currency = $7,
    target_date = sqlc.narg('target_date'), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteGoal :execrows
UPDATE goals SET deleted_at = now(), updated_at = now() WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: SetGoalAchieved :exec
UPDATE goals SET achieved_at = sqlc.narg('achieved_at') WHERE id = $1;

-- name: ListAllocationsForGoals :many
SELECT a.*, w.name AS wallet_name, w.icon AS wallet_icon, w.currency AS wallet_currency
FROM goal_allocations a
JOIN wallets w ON w.id = a.wallet_id
WHERE a.goal_id = ANY($1::uuid[])
ORDER BY a.goal_id, a.updated_at, a.id;

-- name: UpsertAllocation :exec
INSERT INTO goal_allocations (goal_id, wallet_id, amount) VALUES ($1, $2, $3)
ON CONFLICT (goal_id, wallet_id) DO UPDATE SET amount = EXCLUDED.amount, updated_at = now();

-- name: DeleteAllocation :exec
DELETE FROM goal_allocations WHERE goal_id = $1 AND wallet_id = $2;

-- name: DeleteAllocationsForGoal :exec
DELETE FROM goal_allocations WHERE goal_id = $1;

-- name: DeleteAllocationsForWallet :exec
DELETE FROM goal_allocations WHERE wallet_id = $1;

-- name: ReservedByWallet :many
-- Reserved amount per wallet of a space (every allocation belongs to a live goal).
SELECT a.wallet_id, sum(a.amount)::numeric(28,8) AS reserved
FROM goal_allocations a
JOIN goals g ON g.id = a.goal_id AND g.deleted_at IS NULL
WHERE g.space_id = $1
GROUP BY a.wallet_id;

-- name: ReservedForWallet :one
SELECT COALESCE(sum(a.amount), 0)::numeric(28,8)
FROM goal_allocations a
JOIN goals g ON g.id = a.goal_id AND g.deleted_at IS NULL
WHERE a.wallet_id = $1;

-- name: ReservedForWalletExcludingGoal :one
SELECT COALESCE(sum(a.amount), 0)::numeric(28,8)
FROM goal_allocations a
JOIN goals g ON g.id = a.goal_id AND g.deleted_at IS NULL
WHERE a.wallet_id = $1 AND a.goal_id <> $2;

-- name: CountAllocationsForWallet :one
SELECT count(*)::int FROM goal_allocations WHERE wallet_id = $1;
