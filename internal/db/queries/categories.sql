-- name: ListCategories :many
SELECT * FROM categories
WHERE space_id = $1 AND deleted_at IS NULL
  AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
ORDER BY type, sort_order, created_at, id;

-- name: GetCategory :one
SELECT * FROM categories WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: NextCategorySortOrder :one
SELECT COALESCE(max(sort_order) + 1, 0)::int FROM categories
WHERE space_id = $1 AND type = $2 AND deleted_at IS NULL;

-- name: CreateCategory :one
INSERT INTO categories (space_id, type, name, icon, color, budget_amount, sort_order, created_by_id, system_key)
VALUES ($1, $2, $3, $4, $5, sqlc.narg('budget_amount'), $6, $7, sqlc.narg('system_key'))
RETURNING *;

-- name: UpdateCategory :one
UPDATE categories SET
    type          = COALESCE(sqlc.narg('type'), type),
    name          = COALESCE(sqlc.narg('name'), name),
    icon          = COALESCE(sqlc.narg('icon'), icon),
    color         = COALESCE(sqlc.narg('color'), color),
    budget_amount = CASE WHEN sqlc.arg('set_budget')::bool THEN sqlc.narg('budget_amount') ELSE budget_amount END,
    updated_at    = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteCategory :execrows
UPDATE categories SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND space_id = $2 AND deleted_at IS NULL;

-- name: ListCategoryIDsByType :many
SELECT id FROM categories
WHERE space_id = $1 AND type = $2 AND deleted_at IS NULL
ORDER BY sort_order, created_at, id
FOR UPDATE;

-- name: SetCategorySortOrder :exec
UPDATE categories SET sort_order = $3, updated_at = now() WHERE id = $1 AND space_id = $2;
