-- +goose Up
-- Phase F (T12, T13): a stable key for the 11 default categories so clients can localize them,
-- and distinct default icons.
ALTER TABLE categories ADD COLUMN system_key text;

-- +goose StatementBegin
DO $$
DECLARE
    keyed integer;
BEGIN
    UPDATE categories c SET system_key = s.key
      FROM (VALUES
        ('EXPENSE', 'transport',     'TRANSPORT'),
        ('EXPENSE', 'shopping',      'SHOPPING'),
        ('EXPENSE', 'subscriptions', 'SUBSCRIPTIONS'),
        ('EXPENSE', 'health',        'HEALTH'),
        ('EXPENSE', 'gifts',         'GIFTS'),
        ('EXPENSE', 'snacks',        'SNACKS'),
        ('EXPENSE', 'family',        'FAMILY'),
        ('INCOME',  'salary',        'SALARY'),
        ('INCOME',  'freelance',     'FREELANCE'),
        ('INCOME',  'investment',    'INVESTMENT'),
        ('INCOME',  'dividend',      'DIVIDEND')
      ) AS s(type, name, key)
     WHERE c.type = s.type AND lower(c.name) = s.name AND c.deleted_at IS NULL;
    GET DIAGNOSTICS keyed = ROW_COUNT;
    RAISE NOTICE 'migration 00018: % default categories got a system_key', keyed;
END $$;
-- +goose StatementEnd

-- The seeds Subscriptions and Dividend shared an icon with the client fallback / another seed.
-- Only untouched default rows are updated (the user's own icon choice is never overwritten).
UPDATE categories SET icon = '🔁' WHERE system_key = 'SUBSCRIPTIONS' AND icon = '✨';
UPDATE categories SET icon = '🏦' WHERE system_key = 'DIVIDEND' AND icon = '📈';

CREATE UNIQUE INDEX categories_space_system_key_live_key
    ON categories (space_id, system_key) WHERE deleted_at IS NULL AND system_key IS NOT NULL;

-- +goose Down
DROP INDEX categories_space_system_key_live_key;
UPDATE categories SET icon = '✨' WHERE system_key = 'SUBSCRIPTIONS' AND icon = '🔁';
UPDATE categories SET icon = '📈' WHERE system_key = 'DIVIDEND' AND icon = '🏦';
ALTER TABLE categories DROP COLUMN system_key;
