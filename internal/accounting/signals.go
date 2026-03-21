package accounting

// Signal channel names used by TenantAccountingWorkflow.
const (
	SignalHeartbeat       = "heartbeat-signal"
	SignalRegisterCluster = "register-cluster-signal"
	QueryLastTBAck        = "last-tb-ack"
	// QueryExceededResources returns the set of resource types whose last TB
	// batch was rejected with ExceedsCredits. Cleared when credits are issued.
	QueryExceededResources = "exceeded-resources"

	// UpdateIssueCredit is a Temporal Update (request-response) for quota adjustments.
	// Unlike signals, the caller blocks until the TB allocation is confirmed.
	UpdateIssueCredit = "issue-credit"
)

// IssueCreditResult is returned by the UpdateIssueCredit update handler
// once the TigerBeetle allocation transfer has been durably committed.
type IssueCreditResult struct {
	// Balances maps resource_type → TB balance snapshot immediately after the adjustment.
	Balances map[string]QuotaSnapshotData
}

// HeartbeatSignal carries the data from one parsed heartbeat.
type HeartbeatSignal struct {
	ClusterID            string
	ClusterUUID          [16]byte
	SequenceNumber       uint64
	CPUMillisecondsDelta int64
	MemoryMBSecondsDelta int64
	ActiveNodes          int32
	HeartbeatTimestampNs int64
}

// QuotaAdjustmentSignal carries a manual credit issuance.
type QuotaAdjustmentSignal struct {
	ResourceType string
	Amount       int64
	Reason       string
	Code         uint16
}

// RegisterClusterSignal initialises sequence tracking for a new cluster.
type RegisterClusterSignal struct {
	ClusterID string
}
