package domain

// TransferCode is the 16-bit code field on a TigerBeetle transfer.
// Codes are stable identifiers for the type of accounting event.
type TransferCode uint16

const (
	// CodeQuotaAllocation (100): credit the tenant wallet at billing period start.
	// Direction: operator → tenant_quota
	CodeQuotaAllocation TransferCode = 100

	// CodeQuotaAdjustment (101): manual credit issued by support.
	// Direction: operator → tenant_quota
	CodeQuotaAdjustment TransferCode = 101

	// CodeSurgePackCredit (102): surge pack purchased mid-period.
	// Direction: operator → tenant_quota
	CodeSurgePackCredit TransferCode = 102

	// CodeUsageRecord (200): heartbeat usage debit.
	// Direction: tenant_quota → sink
	CodeUsageRecord TransferCode = 200

	// CodeUsageCorrection (201): correcting a prior usage record.
	// Can be positive (refund: operator→tenant_quota) or negative (tenant_quota→sink).
	CodeUsageCorrection TransferCode = 201

	// CodeGaugeReservation (202): pending transfer reserving gauge-type quota.
	// Direction: tenant_quota → sink (PENDING flag set; voided or posted on next state change)
	CodeGaugeReservation TransferCode = 202

	// CodePeriodClose (300): drain balance at billing period end.
	// Direction: tenant_quota → sink  (uses balancing_debit flag)
	CodePeriodClose TransferCode = 300
)
