package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/net/http2"

	creditsystemv1 "github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1"
	"github.com/cshubhamrao/cloud-credit-system/gen/creditsystem/v1/creditsystemv1connect"
	"github.com/cshubhamrao/cloud-credit-system/internal/compress"
)

const defaultServerURL = "http://localhost:8080"
const defaultDebugURL = "http://localhost:6061"

func main() {
	serverURL := os.Getenv("SERVER_URL")
	if serverURL == "" {
		serverURL = defaultServerURL
	}
	debugURL := os.Getenv("DEBUG_URL")
	if debugURL == "" {
		debugURL = defaultDebugURL
	}

	httpClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}

	// Tell the server we can decode zstd responses — it will prefer zstd over
	// gzip when sending quota info / ACKs back to us.
	// We do NOT set WithSendCompression: heartbeat requests are ~100 bytes and
	// compressing them costs more CPU than it saves on the wire.
	zstdOpts := []connect.ClientOption{
		connect.WithGRPC(),
		connect.WithAcceptCompression(
			compress.Name,
			func() connect.Decompressor { return compress.NewDecompressor() },
			func() connect.Compressor { return compress.NewCompressor() },
		),
	}

	provClient := creditsystemv1connect.NewProvisioningServiceClient(httpClient, serverURL, zstdOpts...)
	adminClient := creditsystemv1connect.NewAdminServiceClient(httpClient, serverURL, zstdOpts...)
	hbClient := creditsystemv1connect.NewHeartbeatServiceClient(httpClient, serverURL, zstdOpts...)

	driver := NewScenarioDriver(provClient, adminClient, hbClient)
	model := initialModel(driver, debugURL)
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	driver.program = p

	if _, err := p.Run(); err != nil {
		slog.Error("TUI error", "error", err)
		os.Exit(1)
	}
}

// ─── ClusterSim ───────────────────────────────────────────────────────────────

// ClusterSim represents one simulated workload cluster.
type ClusterSim struct {
	id        string
	label     string
	tenantIdx int

	tickInterval time.Duration
	usageMult    float64

	// exhaustCh is closed when step 4 (drive to exhaustion) begins.
	exhaustCh chan struct{}

	// replayCh carries old sequence numbers to re-send for the idempotency demo.
	replayCh chan uint64

	// recentSeqs holds the last few sent sequence numbers for replay.
	recentSeqs []uint64

	// stopped is set true when the server sends a TYPE_SHUTDOWN kill command.
	// The send loop skips the entire heartbeat while stopped.
	// Cleared automatically when the server stops sending kill commands (credits restored).
	stopped atomic.Bool
}

func newClusterSim(label string, tenantIdx int, tickInterval time.Duration, usageMult float64) *ClusterSim {
	return &ClusterSim{
		label:        label,
		tenantIdx:    tenantIdx,
		tickInterval: tickInterval,
		usageMult:    usageMult,
		exhaustCh:    make(chan struct{}),
		replayCh:     make(chan uint64, 8),
	}
}

// rateDesc returns a human-readable description of the heartbeat rate.
func rateDesc(d time.Duration) string {
	if d < time.Second {
		rate := int(time.Second / d)
		return fmt.Sprintf("%d hb/s", rate)
	}
	secs := int(d.Seconds())
	if secs <= 1 {
		return "1 hb/s"
	}
	return fmt.Sprintf("1 hb/%ds", secs)
}

// run opens a bidi HeartbeatStream and drives it like a real cluster agent would.
// It sends bubbletea messages to keep the TUI in sync with real server responses.
func (s *ClusterSim) run(d *ScenarioDriver) {
	if s.id == "" {
		return
	}

	ctx := context.Background()
	stream := d.hb.HeartbeatStream(ctx)
	defer stream.CloseRequest()

	var (
		seq     uint64
		lastAck uint64
		prevAck uint64
		exhaust bool
	)

	tenant := d.tenants[s.tenantIdx]

	// Recv goroutine — parse real HeartbeatResponse from server.
	go func() {
		wasStopped := false
		for {
			resp, err := stream.Receive()
			if err != nil {
				return
			}
			lastAck = resp.AckSequence

			// Count new TB transfers: each acked seq = 3 transfers (cpu + mem + nodes).
			if resp.AckSequence > prevAck {
				tenant.recordTransfers(d.program, int(resp.AckSequence-prevAck)*3)
				prevAck = resp.AckSequence
			}

			// Build quota update from real server response.
			if len(resp.Quotas) > 0 {
				quotas := make([]QuotaState, 0, len(resp.Quotas))
				for _, q := range resp.Quotas {
					pct := 0.0
					if q.Limit > 0 {
						pct = float64(q.Used) / float64(q.Limit) * 100
					}
					status := "ok"
					switch q.Status {
					case creditsystemv1.Status_STATUS_QUOTA_WARNING:
						status = "warning"
					case creditsystemv1.Status_STATUS_QUOTA_EXCEEDED:
						status = "exceeded"
					}
					quotas = append(quotas, QuotaState{
						Resource:  q.ResourceType,
						Used:      q.Used,
						Limit:     q.Limit,
						Available: q.Available,
						Pct:       pct,
						Status:    status,
					})
				}
				d.program.Send(quotaUpdateMsg{TenantIdx: s.tenantIdx, Quotas: quotas})
			}

			// Smart-client kill signal: server sends TYPE_SHUTDOWN commands when quota
			// is exceeded. Stop all heartbeats while the signal is active; auto-resume
			// when the server stops sending it (credits restored).
			hasKill := false
			for _, cmd := range resp.PendingCommands {
				if cmd.Type == creditsystemv1.Command_TYPE_SHUTDOWN {
					hasKill = true
					break
				}
			}
			if hasKill && !wasStopped {
				wasStopped = true
				s.stopped.Store(true)
				tenant.recordRejected(d.program)
				d.program.Send(eventMsg{
					Timestamp: time.Now(),
					TenantIdx: s.tenantIdx,
					Kind:      EventHardLimit,
					Message:   fmt.Sprintf("[%s] KILL SIGNAL — stopping all heartbeats (hard limit hit)", s.label),
				})
			} else if !hasKill && wasStopped {
				wasStopped = false
				s.stopped.Store(false)
				d.program.Send(eventMsg{
					Timestamp: time.Now(),
					TenantIdx: s.tenantIdx,
					Kind:      EventInfo,
					Message:   fmt.Sprintf("[%s] RESUMED — quota restored, heartbeats resuming", s.label),
				})
			}

			// Log ACK in event log.
			kind := EventHeartbeatACK
			evMsg := fmt.Sprintf("[%s] ack seq=%d", s.label, resp.AckSequence)
			if resp.Status == creditsystemv1.Status_STATUS_QUOTA_WARNING {
				kind = EventSoftLimit
				evMsg = fmt.Sprintf("[%s] SOFT LIMIT — approaching quota", s.label)
			}
			d.program.Send(eventMsg{Timestamp: time.Now(), TenantIdx: s.tenantIdx, Kind: kind, Message: evMsg})

			// Update cluster state.
			d.program.Send(clusterHeartbeatMsg{
				tenantIdx: s.tenantIdx,
				label:     s.label,
				seq:       seq,
				ackSeq:    resp.AckSequence,
				lastSeen:  time.Now(),
			})
		}
	}()

	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.exhaustCh:
			exhaust = true
			s.exhaustCh = make(chan struct{}) // re-arm (won't fire again)

		case replaySeq := <-s.replayCh:
			// Send a duplicate sequence number — server should dedup it.
			_ = stream.Send(&creditsystemv1.HeartbeatRequest{
				ClusterId:            s.id,
				SequenceNumber:       replaySeq,
				LastAckSequence:      lastAck,
				CpuMillisecondsDelta: 1000,
				MemoryMbSecondsDelta: 500,
				ActiveNodes:          4,
			})
			tenant.recordIdempotent(d.program)
			d.program.Send(eventMsg{
				Timestamp: time.Now(),
				TenantIdx: s.tenantIdx,
				Kind:      EventDuplicate,
				Message:   fmt.Sprintf("[%s] replaying seq=%d — deduped at workflow (processedSeqs)", s.label, replaySeq),
			})

		case <-ticker.C:
			if d.paused.Load() {
				continue
			}
			// Smart client: server issued a kill signal — hold heartbeats until resumed.
			if s.stopped.Load() {
				continue
			}
			seq++

			// Track recent seqs for idempotency replay.
			s.recentSeqs = append(s.recentSeqs, seq)
			if len(s.recentSeqs) > 10 {
				s.recentSeqs = s.recentSeqs[len(s.recentSeqs)-10:]
			}

			tickMs := int64(s.tickInterval / time.Millisecond)

			// Active nodes (gauge): current point-in-time count.
			nodes := int32(rand.N(4) + 2) // 2–5 nodes at normal load

			if exhaust {
				// Attempt to scale far beyond the per-tenant node limit (20 for Acme).
				// TigerBeetle will reject the pending gauge transfer (ExceedsCredits).
				nodes = int32(rand.N(5) + 18) // 18–22 — exceeds limit on its own
			}

			// cpu_milliseconds_delta = elapsed_ms × vCPUs_per_node × nodes × utilization.
			// 2 vCPUs per node at 50–90% utilisation represents a realistic cloud instance.
			// usageMult scales relative workload intensity across clusters.
			const vCPUsPerNode = 2
			util     := 0.5 + rand.Float64()*0.4 // 50–90% CPU utilisation
			cpuDelta := int64(float64(tickMs) * vCPUsPerNode * float64(nodes) * util * s.usageMult)

			// memory_mb_seconds_delta = elapsed_s × mem_MB_per_node × nodes.
			// 512 MB per node (0.5 GB); multiply by nodes so memory scales with cluster size.
			const memMBPerNode = 512
			memDelta := int64(float64(tickMs) / 1000.0 * memMBPerNode * float64(nodes) * s.usageMult)

			if exhaust {
				// Peg all CPUs at burst capacity (~8× normal vCPU load) to rapidly
				// exhaust the cumulative CPU quota.
				cpuDelta = int64(float64(tickMs) * vCPUsPerNode * 8 * float64(nodes) * s.usageMult)
			}

			err := stream.Send(&creditsystemv1.HeartbeatRequest{
				ClusterId:            s.id,
				SequenceNumber:       seq,
				LastAckSequence:      lastAck,
				CpuMillisecondsDelta: cpuDelta,
				MemoryMbSecondsDelta: memDelta,
				ActiveNodes:          nodes,
			})
			if err != nil {
				slog.Warn("stream send error", "cluster", s.label, "error", err)
				return
			}
			slog.Debug("heartbeat sent", "cluster", s.label, "seq", seq,
				"tickMs", tickMs, "nodes", nodes, "cpu", cpuDelta, "mem", memDelta, "exhaust", exhaust)
		}
	}
}

// ─── TenantSim ───────────────────────────────────────────────────────────────

// TenantSim holds per-tenant simulation state and TB stats.
type TenantSim struct {
	idx      int
	tenantID string
	name     string
	tier     string
	clusters []*ClusterSim

	tbMu    sync.Mutex
	tbStats TBStats
}

func (t *TenantSim) recordTransfers(p *tea.Program, n int) {
	t.tbMu.Lock()
	t.tbStats.TransfersTotal += n
	s := t.tbStats
	t.tbMu.Unlock()
	p.Send(tbStatsUpdateMsg{TenantIdx: t.idx, Stats: s})
}

func (t *TenantSim) recordRejected(p *tea.Program) {
	t.tbMu.Lock()
	t.tbStats.Rejected++
	s := t.tbStats
	t.tbMu.Unlock()
	p.Send(tbStatsUpdateMsg{TenantIdx: t.idx, Stats: s})
}

func (t *TenantSim) recordIdempotent(p *tea.Program) {
	t.tbMu.Lock()
	t.tbStats.Idempotent++
	s := t.tbStats
	t.tbMu.Unlock()
	p.Send(tbStatsUpdateMsg{TenantIdx: t.idx, Stats: s})
}

// ─── ScenarioDriver ──────────────────────────────────────────────────────────

type ScenarioDriver struct {
	prov  creditsystemv1connect.ProvisioningServiceClient
	admin creditsystemv1connect.AdminServiceClient
	hb    creditsystemv1connect.HeartbeatServiceClient

	tenantID string // Acme Corp's tenant ID (used for admin API calls)
	tenants  []*TenantSim

	paused  atomic.Bool
	program *tea.Program
}

func NewScenarioDriver(
	prov creditsystemv1connect.ProvisioningServiceClient,
	admin creditsystemv1connect.AdminServiceClient,
	hb creditsystemv1connect.HeartbeatServiceClient,
) *ScenarioDriver {
	// Tick intervals tuned for ~100 msg/s cumulative:
	// A: 25ms=40/s, B: 50ms=20/s, C: 100ms=10/s, D: 50ms=20/s, E: 100ms=10/s → 100/s
	acme := &TenantSim{
		idx:  0,
		name: "Acme Corp",
		tier: "pro",
		clusters: []*ClusterSim{
			newClusterSim("A", 0, 25*time.Millisecond, 1.0),
			newClusterSim("B", 0, 50*time.Millisecond, 0.6),
			newClusterSim("C", 0, 100*time.Millisecond, 0.3),
		},
	}
	globex := &TenantSim{
		idx:  1,
		name: "Globex Inc",
		tier: "starter",
		clusters: []*ClusterSim{
			newClusterSim("D", 1, 50*time.Millisecond, 0.3),
			newClusterSim("E", 1, 100*time.Millisecond, 0.15),
		},
	}
	return &ScenarioDriver{
		prov:    prov,
		admin:   admin,
		hb:      hb,
		tenants: []*TenantSim{acme, globex},
	}
}

func (d *ScenarioDriver) StartCmd() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(500 * time.Millisecond)
		return d.runStep(StepProvision)
	}
}

func (d *ScenarioDriver) StepCmd(step int) tea.Cmd {
	return func() tea.Msg {
		return d.runStep(step)
	}
}

func (d *ScenarioDriver) runStep(step int) tea.Msg {
	ctx := context.Background()
	switch step {
	case StepProvision:
		return d.stepProvision(ctx)
	case StepStartClusters:
		return d.stepStartClusters(ctx)
	case StepRecordUsage:
		return eventMsg{Timestamp: time.Now(), TenantIdx: 0, Kind: EventInfo, Message: "Recording usage — quota bars filling from real heartbeats..."}
	case StepExhaust:
		return d.stepExhaust()
	case StepSurgePack:
		return d.stepSurgePack(ctx)
	case StepIdempotency:
		return d.stepIdempotency()
	case StepSummary:
		return eventMsg{Timestamp: time.Now(), TenantIdx: 0, Kind: EventInfo, Message: "Demo complete — all success criteria met."}
	}
	return nil
}

func errMsg(tenantIdx int, label string, err error) eventMsg {
	return eventMsg{Timestamp: time.Now(), TenantIdx: tenantIdx, Kind: EventInfo,
		Message: fmt.Sprintf("ERROR [%s]: %v", label, err)}
}

func (d *ScenarioDriver) stepProvision(ctx context.Context) tea.Msg {
	acme := d.tenants[0]
	respA, err := d.prov.RegisterTenant(ctx, connect.NewRequest(&creditsystemv1.RegisterTenantRequest{
		Name:                 "Acme Corp",
		BillingTier:          "pro",
		CpuHoursCredits:      1_500_000,
		MemoryGbHoursCredits: 1_000_000,
		ActiveNodesLimit:     20,
	}))
	if err != nil {
		return errMsg(0, "RegisterTenant Acme", err)
	}
	acme.tenantID = respA.Msg.TenantId
	d.tenantID = acme.tenantID

	globex := d.tenants[1]
	respG, err := d.prov.RegisterTenant(ctx, connect.NewRequest(&creditsystemv1.RegisterTenantRequest{
		Name:                 "Globex Inc",
		BillingTier:          "starter",
		CpuHoursCredits:      500_000,
		MemoryGbHoursCredits: 500_000,
		ActiveNodesLimit:     10,
	}))
	if err != nil {
		return errMsg(1, "RegisterTenant Globex", err)
	}
	globex.tenantID = respG.Msg.TenantId

	d.program.Send(eventMsg{Timestamp: time.Now(), TenantIdx: 1, Kind: EventInfo,
		Message: fmt.Sprintf("Tenant Globex Inc provisioned — wallets loaded tenant=%s", globex.tenantID[:8])})
	return eventMsg{Timestamp: time.Now(), TenantIdx: 0, Kind: EventInfo,
		Message: fmt.Sprintf("Tenant Acme Corp provisioned — wallets loaded tenant=%s", acme.tenantID[:8])}
}

func (d *ScenarioDriver) stepStartClusters(ctx context.Context) tea.Msg {
	acme := d.tenants[0]
	globex := d.tenants[1]

	type clusterConfig struct {
		sim    *ClusterSim
		region string
	}

	acmeClusters := []clusterConfig{
		{acme.clusters[0], "us-east-1"},
		{acme.clusters[1], "eu-west-1"},
		{acme.clusters[2], "ap-south-1"},
	}
	globexClusters := []clusterConfig{
		{globex.clusters[0], "us-west-2"},
		{globex.clusters[1], "ca-central-1"},
	}

	allClusters := []struct {
		cfg       clusterConfig
		tenantID  string
		tenantIdx int
	}{}
	for _, cc := range acmeClusters {
		allClusters = append(allClusters, struct {
			cfg       clusterConfig
			tenantID  string
			tenantIdx int
		}{cc, acme.tenantID, 0})
	}
	for _, cc := range globexClusters {
		allClusters = append(allClusters, struct {
			cfg       clusterConfig
			tenantID  string
			tenantIdx int
		}{cc, globex.tenantID, 1})
	}

	for _, cl := range allClusters {
		r, err := d.prov.RegisterCluster(ctx, connect.NewRequest(&creditsystemv1.RegisterClusterRequest{
			TenantId:      cl.tenantID,
			CloudProvider: "aws",
			Region:        cl.cfg.region,
		}))
		if err != nil {
			return errMsg(cl.tenantIdx, fmt.Sprintf("RegisterCluster %s", cl.cfg.sim.label), err)
		}
		cl.cfg.sim.id = r.Msg.ClusterId
	}

	// Start all bidi stream goroutines
	for _, sim := range acme.clusters {
		go sim.run(d)
	}
	for _, sim := range globex.clusters {
		go sim.run(d)
	}

	// Send Globex cluster update
	d.program.Send(clusterUpdateMsg{
		TenantIdx: 1,
		Clusters: []ClusterState{
			{Name: "Cluster D", Region: "us-west-2", Speed: "fast", Connected: true, RateDesc: rateDesc(50 * time.Millisecond)},
			{Name: "Cluster E", Region: "ca-central-1", Speed: "slow", Connected: true, RateDesc: rateDesc(100 * time.Millisecond)},
		},
	})

	// Return Acme cluster update as the primary message
	return clusterUpdateMsg{
		TenantIdx: 0,
		Clusters: []ClusterState{
			{Name: "Cluster A", Region: "us-east-1", Speed: "fast", Connected: true, RateDesc: rateDesc(25 * time.Millisecond)},
			{Name: "Cluster B", Region: "eu-west-1", Speed: "medium", Connected: true, RateDesc: rateDesc(50 * time.Millisecond)},
			{Name: "Cluster C", Region: "ap-south-1", Speed: "slow", Connected: true, RateDesc: rateDesc(100 * time.Millisecond)},
		},
	}
}

func (d *ScenarioDriver) stepExhaust() tea.Msg {
	// Clear any previous kill-stop so clusters are live before ramping.
	for _, sim := range d.tenants[0].clusters {
		sim.stopped.Store(false)
	}
	// Signal Cluster A (Acme's first cluster) to ramp up usage aggressively.
	close(d.tenants[0].clusters[0].exhaustCh)
	return eventMsg{
		Timestamp: time.Now(),
		TenantIdx: 0,
		Kind:      EventSoftLimit,
		Message:   "Cluster A ramping CPU — driving toward hard limit...",
	}
}

func (d *ScenarioDriver) stepSurgePack(ctx context.Context) tea.Msg {
	_, err := d.admin.IssueTenantCredit(ctx, connect.NewRequest(&creditsystemv1.IssueTenantCreditRequest{
		TenantId:     d.tenantID,
		ResourceType: "cpu_hours",
		Amount:       500_000,
		Reason:       "Emergency surge pack",
	}))
	msg := "Surge pack applied — CPU wallet +500K credits; clusters will auto-resume"
	if err != nil {
		msg = fmt.Sprintf("Surge pack error: %v", err)
	}
	// No explicit unblock needed: the server stops sending TYPE_SHUTDOWN once
	// ExceededResources is cleared, and the recv goroutine auto-resumes the cluster.
	return eventMsg{Timestamp: time.Now(), TenantIdx: 0, Kind: EventSurgePack, Message: msg}
}

func (d *ScenarioDriver) stepIdempotency() tea.Msg {
	// Replay 3 recent sequences from Cluster A on its real stream.
	// The workflow's processedSeqs set will reject them — no double-charging.
	simA := d.tenants[0].clusters[0]
	seqs := simA.recentSeqs
	if len(seqs) > 3 {
		seqs = seqs[:3]
	}
	for _, seq := range seqs {
		simA.replayCh <- seq
	}
	return eventMsg{
		Timestamp: time.Now(),
		TenantIdx: 0,
		Kind:      EventInfo,
		Message:   fmt.Sprintf("Replaying %d sequences on real stream — dedup enforced by processedSeqs", len(seqs)),
	}
}

// serverStatsPollCmd polls /debug/stats every 2s and sends serverStatsMsg.
// Self-perpetuating: Update re-issues this cmd on each received message.
func serverStatsPollCmd(debugURL string) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(2 * time.Second)
		resp, err := http.Get(debugURL + "/debug/stats") //nolint:noctx
		if err != nil {
			return serverStatsMsg{Ok: false}
		}
		defer resp.Body.Close()
		var s ServerStats
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return serverStatsMsg{Ok: false}
		}
		s.Ok = true
		return serverStatsMsg(s)
	}
}

// clusterHeartbeatMsg updates a specific cluster's live state.
type clusterHeartbeatMsg struct {
	tenantIdx int
	label     string
	seq       uint64
	ackSeq    uint64
	lastSeen  time.Time
}
