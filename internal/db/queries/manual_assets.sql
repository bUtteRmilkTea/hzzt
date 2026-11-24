-- name: CreateManualAsset :one
INSERT INTO manual_assets (space_id, kind, name, icon, currency, current_value, counterparty_name, due_date,
                           note, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('counterparty_name'), sqlc.narg('due_date'),
        sqlc.narg('note'), $7)
RETURNING *;

-- name: GetManualAsset :one
SELECT * FROM manual_assets WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: GetManualAssetForUpdate :one
SELECT * FROM manual_assets WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL FOR UPDATE;

-- name: GetManualAssetAnyForUpdate :one
-- Including a soft-deleted asset (to reverse a settlement of a deleted asset).
SELECT * FROM manual_assets WHERE id = $1 AND space_id = $2 FOR UPDATE;

-- name: ListManualAssets :many
SELECT * FROM manual_assets
WHERE space_id = $1 AND deleted_at IS NULL
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
ORDER BY kind, created_at, id;

-- name: UpdateManualAsset :one
-- Full replacement of the editable columns: the service computes the final values.
UPDATE manual_assets SET
    name = $3, icon = $4, currency = $5, current_value = $6,
    counterparty_name = sqlc.narg('counterparty_name'), due_date = sqlc.narg('due_date'),
    note = sqlc.narg('note'),
    updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SetManualAssetValue :exec
UPDATE manual_assets SET current_value = $2, updated_at = now() WHERE id = $1;

-- name: SoftDeleteManualAsset :execrows
UPDATE manual_assets SET deleted_at = now(), updated_at = now() WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: InsertValueChange :exec
INSERT INTO manual_asset_value_changes (asset_id, previous_value, new_value, reason, transaction_id)
VALUES ($1, $2, $3, $4, sqlc.narg('transaction_id'));

-- name: ListValueChanges :many
SELECT * FROM manual_asset_value_changes WHERE asset_id = $1 ORDER BY changed_at DESC, id DESC;

-- name: GetValueChange :one
SELECT * FROM manual_asset_value_changes WHERE id = $1 AND asset_id = $2;

-- name: NewestValueChange :one
SELECT * FROM manual_asset_value_changes WHERE asset_id = $1 ORDER BY changed_at DESC, id DESC LIMIT 1;

-- name: DeleteValueChange :exec
DELETE FROM manual_asset_value_changes WHERE id = $1 AND asset_id = $2;

-- name: ManualAssetHasSettlements :one
SELECT EXISTS (SELECT 1 FROM transactions WHERE manual_asset_id = $1);
