-- name: InsertCluster :one
INSERT INTO workload_clusters (id, tenant_id, cloud_provider, region)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCluster :one
SELECT * FROM workload_clusters WHERE id = $1;

-- name: ListClustersByTenant :many
SELECT * FROM workload_clusters
WHERE tenant_id = $1 AND deregistered_at IS NULL
ORDER BY created_at;

-- name: UpdateClusterHeartbeat :exec
UPDATE workload_clusters
SET last_heartbeat = NOW(), status = 'healthy'
WHERE id = $1;
