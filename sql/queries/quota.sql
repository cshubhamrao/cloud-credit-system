-- name: InsertTBAccountMapping :exec
INSERT INTO tb_account_ids (tenant_id, resource_type, account_type, tb_account_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, resource_type, account_type) DO NOTHING;

-- name: GetTBAccountIDs :many
SELECT * FROM tb_account_ids WHERE tenant_id = $1;

-- name: InsertGlobalTBAccount :exec
INSERT INTO tb_global_account_ids (resource_type, account_type, tb_account_id)
VALUES ($1, $2, $3)
ON CONFLICT (resource_type, account_type) DO NOTHING;

-- name: GetGlobalTBAccount :one
SELECT tb_account_id FROM tb_global_account_ids
WHERE resource_type = $1 AND account_type = $2;

-- name: GetGlobalTBAccounts :many
SELECT * FROM tb_global_account_ids;

-- name: UpsertQuotaSnapshot :exec
INSERT INTO quota_snapshots (tenant_id, resource_type, credits_total, debits_posted, debits_pending, snapshot_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (tenant_id, resource_type) DO UPDATE SET
  credits_total  = EXCLUDED.credits_total,
  debits_posted  = EXCLUDED.debits_posted,
  debits_pending = EXCLUDED.debits_pending,
  snapshot_at    = NOW();

-- name: GetQuotaSnapshots :many
SELECT * FROM quota_snapshots WHERE tenant_id = $1;

-- name: InsertQuotaConfig :exec
INSERT INTO quota_configs (tenant_id, resource_type, hard_limit, soft_limit)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, resource_type) DO NOTHING;

-- name: InsertCreditAdjustment :exec
INSERT INTO credit_adjustments (tenant_id, resource_type, amount, reason, tb_transfer_id)
VALUES ($1, $2, $3, $4, $5);

-- name: ListQuotaConfigsByTenant :many
SELECT * FROM quota_configs WHERE tenant_id = $1;

-- name: MarkSoftLimitAlertSent :exec
UPDATE quota_configs
SET soft_limit_alert_sent = TRUE
WHERE tenant_id = $1 AND resource_type = $2;

-- name: ResetSoftLimitAlert :exec
UPDATE quota_configs
SET soft_limit_alert_sent = FALSE
WHERE tenant_id = $1 AND resource_type = $2;

-- name: ListCreditAdjustmentsByTenant :many
SELECT * FROM credit_adjustments
WHERE tenant_id = $1
ORDER BY created_at DESC;

-- name: GetAllQuotaSnapshots :many
SELECT * FROM quota_snapshots
ORDER BY tenant_id, resource_type;
