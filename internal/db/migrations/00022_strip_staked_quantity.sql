-- +goose Up
-- The staked quantity is removed from crypto details; the whole quantity can now be sold. Reads ignore unknown keys,
-- so this is tidy-up. The notice lists every row that had an amount above zero (the down migration cannot restore it).
-- +goose StatementBegin
DO $$
DECLARE
    r record;
    n integer := 0;
BEGIN
    FOR r IN
        SELECT space_id, id, details->>'stakedQuantity' AS staked FROM investments
        WHERE details ? 'stakedQuantity' AND (details->>'stakedQuantity') ~ '^[0-9.]+$' AND (details->>'stakedQuantity')::numeric > 0
    LOOP
        n := n + 1;
        RAISE NOTICE 'migration 00022 up: space % investment % had staked quantity %', r.space_id, r.id, r.staked;
    END LOOP;
    RAISE NOTICE 'migration 00022 up: % rows had a staked quantity above zero', n;
END $$;
-- +goose StatementEnd
UPDATE investments SET details = details - 'stakedQuantity' WHERE details ? 'stakedQuantity';

-- +goose Down
-- The stored amounts cannot be restored (see the notices of the up migration).
SELECT 1;
