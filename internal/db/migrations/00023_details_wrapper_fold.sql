-- +goose Up
-- The broker, exchange or platform is kept once, in `wrapper`. Rows with an empty wrapper take the first non-empty of
-- details.exchangeOrWallet, details.platform, details.broker (whitespace normalised, at most 60 characters); then the
-- three keys are removed from details. When a row held two different values, the first wins and the notice lists the rest.
-- +goose StatementBegin
DO $$
DECLARE
    r record;
    folded integer := 0;
    stripped integer := 0;
    v text;
BEGIN
    FOR r IN
        SELECT id, space_id, wrapper,
               nullif(regexp_replace(btrim(details->>'exchangeOrWallet'), '\s+', ' ', 'g'), '') AS a,
               nullif(regexp_replace(btrim(details->>'platform'), '\s+', ' ', 'g'), '') AS b,
               nullif(regexp_replace(btrim(details->>'broker'), '\s+', ' ', 'g'), '') AS c
        FROM investments
        WHERE details ?| array['exchangeOrWallet', 'platform', 'broker']
    LOOP
        stripped := stripped + 1;
        v := coalesce(r.a, r.b, r.c);
        IF (r.wrapper IS NULL OR btrim(r.wrapper) = '') AND v IS NOT NULL THEN
            UPDATE investments SET wrapper = left(v, 60) WHERE id = r.id;
            folded := folded + 1;
        END IF;
        IF (r.a IS NOT NULL AND r.b IS NOT NULL AND r.a <> r.b)
           OR (r.a IS NOT NULL AND r.c IS NOT NULL AND r.a <> r.c)
           OR (r.b IS NOT NULL AND r.c IS NOT NULL AND r.b <> r.c) THEN
            RAISE NOTICE 'migration 00023 up: space % investment % had different values (exchangeOrWallet=%, platform=%, broker=%), kept %',
                r.space_id, r.id, r.a, r.b, r.c, coalesce(nullif(btrim(r.wrapper), ''), left(v, 60));
        END IF;
    END LOOP;
    UPDATE investments SET details = details - 'exchangeOrWallet' - 'platform' - 'broker'
        WHERE details ?| array['exchangeOrWallet', 'platform', 'broker'];
    RAISE NOTICE 'migration 00023 up: % rows had one of the three keys, % got their wrapper from it', stripped, folded;
END $$;
-- +goose StatementEnd

-- +goose Down
-- The key move is not reversible (the wrapper column keeps the value; the keys are not written back).
SELECT 1;
