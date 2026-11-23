-- name: GetWalletPref :one
SELECT * FROM user_wallet_prefs WHERE user_id = $1 AND wallet_id = $2;

-- name: ListWalletPrefsInSpace :many
-- The user's preferences for the live wallets of a space.
SELECT p.* FROM user_wallet_prefs p
JOIN wallets w ON w.id = p.wallet_id AND w.deleted_at IS NULL
WHERE p.user_id = $1 AND w.space_id = $2;

-- name: UpsertWalletPref :one
INSERT INTO user_wallet_prefs (user_id, wallet_id, hide_logo, hide_name, hide_type)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, wallet_id)
DO UPDATE SET hide_logo = EXCLUDED.hide_logo, hide_name = EXCLUDED.hide_name, hide_type = EXCLUDED.hide_type, updated_at = now()
RETURNING *;

-- name: DeleteWalletPref :exec
DELETE FROM user_wallet_prefs WHERE user_id = $1 AND wallet_id = $2;
