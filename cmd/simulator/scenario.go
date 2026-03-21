package main

import "time"

// Step IDs
const (
	StepProvision     = 0
	StepStartClusters = 1
	StepRecordUsage   = 2
	StepExhaust       = 3
	StepSurgePack     = 4
	StepIdempotency   = 5
	StepSummary       = 6
)

// ScenarioStep describes one step in the demo.
type ScenarioStep struct {
	Name        string
	Description string
	AutoAdvance bool          // advance without keypress?
	Duration    time.Duration // how long to run before auto-advancing
}

// Scenario is the ordered list of demo steps.
var Scenario = []ScenarioStep{
	{
		Name:        "Provision tenant wallets",
		Description: "Create tenant Acme Corp (pro), provision TB accounts, load credits",
		AutoAdvance: true,
		Duration:    3 * time.Second,
	},
	{
		Name:        "Start clusters streaming",
		Description: "Register Cluster A (us-east-1) and Cluster B (eu-west-1), open bidi streams",
		AutoAdvance: true,
		Duration:    3 * time.Second,
	},
	{
		Name:        "Record usage",
		Description: "Both clusters sending heartbeats — watch quota bars fill. Press N when ready.",
		AutoAdvance: false,
	},
	{
		Name:        "Drive to exhaustion",
		Description: "Cluster A ramps CPU usage — watch hard limit rejection from TigerBeetle. Press N when ready.",
		AutoAdvance: false,
	},
	{
		Name:        "Surge pack top-up",
		Description: "Issue +500K CPU credits — tenant unblocked. Press N when ready.",
		AutoAdvance: false,
	},
	{
		Name:        "Idempotency proof",
		Description: "Replay 3 already-processed sequence numbers — no double-charging. Press N when ready.",
		AutoAdvance: false,
	},
	{
		Name:        "Final summary",
		Description: "All success criteria met",
		AutoAdvance: false,
		Duration:    0,
	},
}

// EventKind classifies an event log entry.
type EventKind int

const (
	EventHeartbeatACK EventKind = iota
	EventUsageDebit
	EventHardLimit
	EventSoftLimit
	EventSurgePack
	EventDuplicate
	EventInfo
)

// Event is one line in the scrolling event log.
type Event struct {
	Timestamp time.Time
	Kind      EventKind
	Message   string
	TenantIdx int
}

// ClusterState holds the live state for one simulated cluster.
type ClusterState struct {
	Name      string
	Region    string
	Speed     string // "fast" | "medium" | "slow"
	Connected bool
	Seq       uint64
	AckSeq    uint64
	RateDesc  string
	LastSeen  time.Time
}

// QuotaState holds the quota bar state for one resource.
type QuotaState struct {
	Resource  string
	Used      int64
	Limit     int64
	Available int64
	Pct       float64
	Status    string // "ok" | "warning" | "exceeded"
}

// TBStats tracks TigerBeetle transfer statistics.
type TBStats struct {
	TransfersTotal  int
	Rejected        int
	Idempotent      int
	LatencyP50Ms    float64
	LatencyP99Ms    float64
	CapacityUsedPct float64
}

// ServerStats holds live Go runtime metrics polled from the server's /debug/stats endpoint.
type ServerStats struct {
	Goroutines   int     `json:"goroutines"`
	HeapAllocMB  float64 `json:"heap_alloc_mb"`
	HeapSysMB    float64 `json:"heap_sys_mb"`
	GCRuns       uint32  `json:"gc_runs"`
	GCPauseUS    uint64  `json:"gc_pause_us"`
	UptimeS      int     `json:"uptime_s"`
	ActiveStreams int     `json:"active_streams"`
	MsgPerSec    float64 `json:"msg_per_sec"`
	MsgTotal     uint64  `json:"msg_total"`
	SignalP50Us  int64   `json:"signal_p50_us"`
	SignalP99Us  int64   `json:"signal_p99_us"`
	Ok           bool    // false if poll failed
}
