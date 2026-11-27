-- name: ListLiveWalletParticipants :many
-- Non-removed participants, owner first, then in invitation order (a stable order the allocation relies on).
SELECT * FROM wallet_participants
WHERE wallet_id = $1 AND status <> 'REMOVED'
ORDER BY (role = 'OWNER') DESC, invited_at, id;

-- name: ListLiveWalletParticipantsForWallets :many
-- Batched form of ListLiveWalletParticipants, for attaching transaction splits to a list of transactions.
SELECT * FROM wallet_participants
WHERE wallet_id = ANY($1::uuid[]) AND status <> 'REMOVED'
ORDER BY wallet_id, (role = 'OWNER') DESC, invited_at, id;

-- name: InsertWalletParticipant :one
INSERT INTO wallet_participants (wallet_id, role, user_id, display_name, email, share_bps, status, invited_at)
VALUES ($1, $2, sqlc.narg('user_id'), $3, sqlc.narg('email'), $4, $5, $6)
RETURNING *;

-- name: UpdateWalletParticipantShare :exec
UPDATE wallet_participants SET display_name = $2, share_bps = $3 WHERE id = $1;

-- name: RemoveWalletParticipant :exec
-- Removing frees the user slot (so the person can be invited again) and detaches the space.
UPDATE wallet_participants SET
    status = 'REMOVED', user_id = NULL, attached_space_id = NULL, responded_at = now()
WHERE id = $1;

-- name: GetWalletParticipant :one
SELECT * FROM wallet_participants WHERE id = $1;

-- name: AcceptWalletInvitation :one
UPDATE wallet_participants SET
    status = 'ACTIVE', user_id = $2, attached_space_id = $3, responded_at = now()
WHERE id = $1 AND status = 'INVITED'
RETURNING *;

-- name: DeclineWalletInvitation :one
UPDATE wallet_participants SET status = 'DECLINED', responded_at = now()
WHERE id = $1 AND status = 'INVITED'
RETURNING *;

-- name: LeaveSharedWallet :one
-- The share stays allocated but becomes a name-only participant.
UPDATE wallet_participants SET
    user_id = NULL, email = NULL, attached_space_id = NULL, status = 'ACTIVE', responded_at = now()
WHERE id = $1 AND role = 'PARTICIPANT' AND status = 'ACTIVE'
RETURNING *;

-- name: SetWalletShared :exec
UPDATE wallets SET is_shared = $2, updated_at = now() WHERE id = $1;

-- name: InsertWalletShareChange :exec
INSERT INTO wallet_share_changes (wallet_id, changed_by_id, before, after) VALUES ($1, $2, $3, $4);

-- name: ListWalletShareChanges :many
SELECT * FROM wallet_share_changes WHERE wallet_id = $1 ORDER BY created_at DESC, id DESC;

-- name: ListInvitationsForUserWallets :many
-- Pending invitations addressed to the user's id or (case-insensitive) account email.
SELECT p.*, w.name AS wallet_name, w.icon AS wallet_icon, w.currency AS wallet_currency,
       o.display_name AS owner_name
FROM wallet_participants p
JOIN wallets w ON w.id = p.wallet_id AND w.deleted_at IS NULL
JOIN wallet_participants o ON o.wallet_id = p.wallet_id AND o.role = 'OWNER' AND o.status <> 'REMOVED'
WHERE p.status = 'INVITED'
  AND (p.user_id = $1 OR lower(p.email::text) = lower($2::text))
ORDER BY p.invited_at DESC, p.id;

-- name: GetWalletInvitationForUser :one
SELECT p.* FROM wallet_participants p
JOIN wallets w ON w.id = p.wallet_id AND w.deleted_at IS NULL
WHERE p.id = $1 AND (p.user_id = $2 OR lower(p.email::text) = lower($3::text)) AND p.status <> 'REMOVED';

-- name: GetMyWalletParticipation :one
-- An ACTIVE participation of the user that can be left.
SELECT p.* FROM wallet_participants p
JOIN wallets w ON w.id = p.wallet_id AND w.deleted_at IS NULL
WHERE p.id = $1 AND p.user_id = $2 AND p.status = 'ACTIVE';

-- name: ListSharedInWallets :many
-- Wallets of other spaces that a participant attached to this space.
SELECT w.*, p.share_bps AS my_share_bps, p.id AS my_participant_id
FROM wallet_participants p
JOIN wallets w ON w.id = p.wallet_id AND w.deleted_at IS NULL AND w.space_id <> p.attached_space_id
WHERE p.attached_space_id = $1 AND p.status = 'ACTIVE' AND p.role = 'PARTICIPANT'
ORDER BY w.created_at, w.id;

-- name: GetWalletByID :one
SELECT * FROM wallets WHERE id = $1 AND deleted_at IS NULL;

-- name: GetWalletForViewer :one
-- A wallet the viewer owns through this space, or that is shared with them and attached to this space.
-- Used only to tell OWNER_ONLY (403) apart from a real 404 when a participant reaches an owner-only
-- route through their own space.
SELECT w.* FROM wallets w
WHERE w.id = $1 AND w.deleted_at IS NULL
  AND (
    w.space_id = $2
    OR EXISTS (
      SELECT 1 FROM wallet_participants p
      WHERE p.wallet_id = w.id AND p.status = 'ACTIVE' AND p.role = 'PARTICIPANT'
        AND p.user_id = $3 AND p.attached_space_id = $2
    )
  );
