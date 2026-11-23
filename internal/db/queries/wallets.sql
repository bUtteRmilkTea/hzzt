-- name: CreateWallet :one
INSERT INTO wallets (space_id, type, name, icon, currency, is_favorite, sort_order, card_style, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetWallet :one
SELECT * FROM wallets WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: GetWalletForUpdate :one
SELECT * FROM wallets WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL FOR UPDATE;

-- name: ListWallets :many
SELECT * FROM wallets
WHERE space_id = $1 AND deleted_at IS NULL
ORDER BY is_favorite DESC, sort_order, created_at, id;

-- name: NextWalletSortOrder :one
SELECT COALESCE(max(sort_order) + 1, 0)::int FROM wallets WHERE space_id = $1 AND deleted_at IS NULL;

-- name: UpdateWallet :one
UPDATE wallets SET
    type        = COALESCE(sqlc.narg('type'), type),
    name        = COALESCE(sqlc.narg('name'), name),
    icon        = COALESCE(sqlc.narg('icon'), icon),
    currency    = COALESCE(sqlc.narg('currency'), currency),
    card_style  = COALESCE(sqlc.narg('card_style'), card_style),
    is_favorite = COALESCE(sqlc.narg('is_favorite'), is_favorite),
    updated_at  = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteWallet :execrows
UPDATE wallets SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: ListWalletIDsForUpdate :many
SELECT id FROM wallets WHERE space_id = $1 AND deleted_at IS NULL ORDER BY id FOR UPDATE;

-- name: SetWalletSortOrder :exec
UPDATE wallets SET sort_order = $3, updated_at = now() WHERE id = $1 AND space_id = $2;

-- name: WalletHasTransactions :one
SELECT EXISTS (
    SELECT 1 FROM transactions WHERE wallet_id = $1 OR to_wallet_id = $1
);

-- name: WalletDeleteImpact :one
-- What a wallet delete leaves behind: every transaction row that points at the wallet (as source or
-- destination), of which the transfers between own wallets. Transactions are hard-deleted rows, so all are live.
SELECT count(*)::int AS transactions,
       (count(*) FILTER (WHERE type = 'TRANSFER' AND to_wallet_id IS NOT NULL AND investment_id IS NULL AND manual_asset_id IS NULL))::int AS transfers
FROM transactions
WHERE wallet_id = $1 OR to_wallet_id = $1;
