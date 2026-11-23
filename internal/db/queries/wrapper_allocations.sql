-- name: ListWrapperAllocations :many
SELECT * FROM investment_wrapper_allocations WHERE space_id = $1 ORDER BY asset_type, wrapper_key;

-- name: GetWrapperAllocation :one
SELECT * FROM investment_wrapper_allocations WHERE space_id = $1 AND asset_type = $2 AND wrapper_key = $3;

-- name: LockWrapperCategory :exec
-- Serialises edits of one asset type's percentages in a space until the transaction ends (also when
-- there are no rows yet to lock).
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0));

-- name: SumWrapperAllocations :one
-- Basis points already assigned in an asset type, leaving one wrapper out.
SELECT COALESCE(sum(pct_bps), 0)::int AS assigned_bps FROM investment_wrapper_allocations
WHERE space_id = $1 AND asset_type = $2 AND wrapper_key <> $3;

-- name: UpsertWrapperAllocation :one
INSERT INTO investment_wrapper_allocations (space_id, asset_type, wrapper_key, label, pct_bps, updated_by_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (space_id, asset_type, wrapper_key)
DO UPDATE SET label = EXCLUDED.label, pct_bps = EXCLUDED.pct_bps, updated_by_id = EXCLUDED.updated_by_id, updated_at = now()
RETURNING *;

-- name: DeleteWrapperAllocation :execrows
DELETE FROM investment_wrapper_allocations WHERE space_id = $1 AND asset_type = $2 AND wrapper_key = $3;

-- name: LatestWrapperLabel :one
-- The most recently written casing of a wrapper among the space's live investments of an asset type.
SELECT wrapper::text AS label FROM investments
WHERE space_id = $1 AND asset_type = $2 AND deleted_at IS NULL AND lower(wrapper) = $3::text
ORDER BY updated_at DESC, id LIMIT 1;
