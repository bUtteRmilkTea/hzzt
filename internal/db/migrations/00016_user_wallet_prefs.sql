-- +goose Up
-- Per-viewer display preferences of a wallet (hide logo / name / type). Synced across the user's
-- devices; private to the user (wallets themselves are shared by the space).
CREATE TABLE user_wallet_prefs (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    wallet_id  uuid        NOT NULL REFERENCES wallets (id) ON DELETE CASCADE,
    hide_logo  boolean     NOT NULL DEFAULT false,
    hide_name  boolean     NOT NULL DEFAULT false,
    hide_type  boolean     NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, wallet_id)
);

CREATE INDEX user_wallet_prefs_wallet_idx ON user_wallet_prefs (wallet_id);

-- +goose Down
DROP TABLE user_wallet_prefs;
