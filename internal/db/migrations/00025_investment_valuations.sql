-- +goose Up
-- Dated valuations of an investment priced by hand (mutual funds): the NAV per unit on a date, with a snapshot of the
-- position at the time so a later BUY or SELL does not rewrite an earlier month.
CREATE TABLE investment_valuations (
    id            uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    investment_id uuid          NOT NULL REFERENCES investments (id) ON DELETE CASCADE,
    as_of         date          NOT NULL,                          -- the NAV date, not the typing date
    nav           numeric(28,8) NOT NULL CHECK (nav >= 0),         -- price per unit on as_of
    quantity      numeric(38,18) NOT NULL,                         -- units held when it was typed
    cost_basis    numeric(28,8) NOT NULL,                          -- quantity x average buy price + fees at that time
    value         numeric(28,8) NOT NULL,                          -- quantity x nav
    note          text          CHECK (note IS NULL OR char_length(note) <= 500),
    created_by_id uuid          NOT NULL REFERENCES users (id),
    created_at    timestamptz   NOT NULL DEFAULT now(),
    UNIQUE (investment_id, as_of)
);

-- +goose Down
DROP TABLE investment_valuations;
