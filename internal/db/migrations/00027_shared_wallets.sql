-- +goose Up
ALTER TABLE wallets ADD COLUMN is_shared boolean NOT NULL DEFAULT false;

CREATE TABLE wallet_participants (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id         uuid        NOT NULL REFERENCES wallets (id) ON DELETE CASCADE,
    role              text        NOT NULL CHECK (role IN ('OWNER', 'PARTICIPANT')),
    -- Null for name-only participants and for invitations not accepted yet.
    user_id           uuid        REFERENCES users (id),
    display_name      text        NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 60),
    email             citext,
    share_bps         integer     NOT NULL CHECK (share_bps BETWEEN 1 AND 10000),
    status            text        NOT NULL CHECK (status IN ('ACTIVE', 'INVITED', 'DECLINED', 'REMOVED')),
    -- The participant's own space that counts their share once they accepted.
    attached_space_id uuid        REFERENCES spaces (id),
    invited_at        timestamptz NOT NULL DEFAULT now(),
    responded_at      timestamptz
);

CREATE UNIQUE INDEX wallet_participants_user_key
    ON wallet_participants (wallet_id, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX wallet_participants_wallet_idx ON wallet_participants (wallet_id) WHERE status <> 'REMOVED';
CREATE INDEX wallet_participants_email_idx ON wallet_participants (lower(email::text)) WHERE status = 'INVITED';
CREATE INDEX wallet_participants_attached_idx ON wallet_participants (attached_space_id) WHERE attached_space_id IS NOT NULL;

-- Audit trail of every change to a wallet's sharing arrangement.
CREATE TABLE wallet_share_changes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id     uuid        NOT NULL REFERENCES wallets (id) ON DELETE CASCADE,
    changed_by_id uuid        NOT NULL REFERENCES users (id),
    before        jsonb       NOT NULL,
    after         jsonb       NOT NULL,
    -- clock_timestamp(), not now(): changes made in one transaction still sort in order.
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX wallet_share_changes_wallet_idx ON wallet_share_changes (wallet_id, created_at DESC);

-- +goose Down
DROP TABLE wallet_share_changes;
DROP TABLE wallet_participants;
ALTER TABLE wallets DROP COLUMN is_shared;
