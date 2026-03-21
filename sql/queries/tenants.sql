-- name: InsertTenant :one
INSERT INTO tenants (id, name, billing_tier)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants WHERE deleted_at IS NULL ORDER BY created_at;

-- name: UpdateTenantStatus :exec
UPDATE tenants SET status = $2 WHERE id = $1;

-- name: DeleteTenant :exec
UPDATE tenants SET status = 'deregistered', deleted_at = NOW() WHERE id = $1;
