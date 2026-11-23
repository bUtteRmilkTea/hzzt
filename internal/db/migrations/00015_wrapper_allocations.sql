-- +goose Up
-- Wrapper names are compared after trimming and collapsing whitespace; make stored data match.
UPDATE investments SET wrapper = regexp_replace(btrim(wrapper), '\s+', ' ', 'g') WHERE wrapper IS NOT NULL AND wrapper <> regexp_replace(btrim(wrapper), '\s+', ' ', 'g');

-- A planned share (percentage) of an asset type held on one wrapper (exchange, broker or platform),
-- edited one wrapper at a time. The percentages of one asset type add up to at most 100.
CREATE TABLE investment_wrapper_allocations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id      uuid        NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    asset_type    text        NOT NULL CHECK (asset_type IN ('CRYPTO','ID_STOCK','US_STOCK','MUTUAL_FUND','GLOBAL_ETF')),
    wrapper_key   text        NOT NULL CHECK (char_length(wrapper_key) BETWEEN 1 AND 60), -- lower(wrapper), whitespace-normalised
    label         text        NOT NULL CHECK (char_length(label) BETWEEN 1 AND 60),
    pct_bps       integer     NOT NULL CHECK (pct_bps BETWEEN 0 AND 10000),
    updated_by_id uuid        NOT NULL REFERENCES users (id),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (space_id, asset_type, wrapper_key)
);

-- +goose Down
DROP TABLE investment_wrapper_allocations;
