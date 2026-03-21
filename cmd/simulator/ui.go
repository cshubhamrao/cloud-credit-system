package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── Styles ──────────────────────────────────────────────────────────────────

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("214"))

	styleDim = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleBlue   = lipgloss.NewStyle().Foreground(lipgloss.Color("69"))
	styleBold   = lipgloss.NewStyle().Bold(true)

	styleHeader = lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(lipgloss.Color("252")).
			Padding(0, 1)

	styleKeyHint = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Italic(true)

	styleTabActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("214"))

	styleTabInactive = lipgloss.NewStyle().
				Foreground(lipgloss.Color("241"))
)

// ─── Messages ─────────────────────────────────────────────────────────────────

type tickMsg time.Time
type stepAdvanceMsg int
type eventMsg Event

type quotaUpdateMsg struct {
	TenantIdx int
	Quotas    []QuotaState
}

type clusterUpdateMsg struct {
	TenantIdx int
	Clusters  []ClusterState
}

type tbStatsUpdateMsg struct {
	TenantIdx int
	Stats     TBStats
}

type serverStatsMsg ServerStats

// ─── Per-tenant UI state ──────────────────────────────────────────────────────

type tenantUIState struct {
	name         string
	tier         string
	quotas       []QuotaState
	clusters     []ClusterState
	tbStats      TBStats
	events       []Event
	progressBars map[string]progress.Model
	eventLog     viewport.Model
}

// ─── Model ────────────────────────────────────────────────────────────────────

// Model is the bubbletea model for the simulator TUI.
type Model struct {
	// Window dimensions
	width  int
	height int

	// Scenario state
	currentStep int
	stepStart   time.Time
	paused      bool
	elapsed     time.Duration

	// Multi-tenant state
	tenants        []tenantUIState
	selectedTenant int // len(tenants) == Server Stats tab

	// Server stats (polled from /debug/stats)
	serverStats ServerStats
	debugURL    string

	// Scenario driver (injected)
	driver *ScenarioDriver
}

func newTenantUIState(name, tier string, quotas []QuotaState) tenantUIState {
	bars := make(map[string]progress.Model)
	for _, q := range quotas {
		bar := progress.New(
			progress.WithDefaultGradient(),
			progress.WithoutPercentage(),
			progress.WithWidth(36),
		)
		bars[q.Resource] = bar
	}
	return tenantUIState{
		name:         name,
		tier:         tier,
		quotas:       quotas,
		progressBars: bars,
		eventLog:     viewport.New(50, 8),
	}
}

func initialModel(driver *ScenarioDriver, debugURL string) Model {
	acme := newTenantUIState("Acme Corp", "pro", []QuotaState{
		{Resource: "cpu_hours", Limit: 1_500_000},
		{Resource: "memory_gb_hours", Limit: 1_000_000},
		{Resource: "active_nodes", Limit: 20},
	})

	globex := newTenantUIState("Globex Inc", "starter", []QuotaState{
		{Resource: "cpu_hours", Limit: 500_000},
		{Resource: "memory_gb_hours", Limit: 500_000},
		{Resource: "active_nodes", Limit: 10},
	})

	return Model{
		currentStep:    0,
		stepStart:      time.Now(),
		driver:         driver,
		tenants:        []tenantUIState{acme, globex},
		selectedTenant: 0,
		debugURL:       debugURL,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), m.driver.StartCmd(), serverStatsPollCmd(m.debugURL))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "Q", "ctrl+c":
			return m, tea.Quit
		case " ":
			m.paused = !m.paused
			m.driver.paused.Store(m.paused)
		case "tab":
			m.selectedTenant = (m.selectedTenant + 1) % (len(m.tenants) + 1)
		case "n", "N":
			next := m.currentStep + 1
			if m.currentStep >= len(Scenario)-1 {
				next = StepExhaust // cycle: summary → exhaust → surge → idempotency → …
			}
			m.currentStep = next
			m.stepStart = time.Now()
			cmds = append(cmds, m.driver.StepCmd(m.currentStep))
		case "r", "R":
			m.currentStep = 0
			m.stepStart = time.Now()
			cmds = append(cmds, m.driver.StepCmd(0))
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		barW := msg.Width/2 - 10
		if barW < 10 {
			barW = 10
		}
		for i := range m.tenants {
			m.tenants[i].eventLog.Width = msg.Width/2 - 4
			m.tenants[i].eventLog.Height = 10
			for k, bar := range m.tenants[i].progressBars {
				bar.Width = barW
				m.tenants[i].progressBars[k] = bar
			}
		}

	case tickMsg:
		if !m.paused {
			m.elapsed = time.Since(m.stepStart)
			step := Scenario[m.currentStep]
			if step.AutoAdvance && step.Duration > 0 && m.elapsed >= step.Duration {
				if m.currentStep < len(Scenario)-1 {
					m.currentStep++
					m.stepStart = time.Now()
					cmds = append(cmds, m.driver.StepCmd(m.currentStep))
				}
			}
		}
		cmds = append(cmds, tickCmd())

	case stepAdvanceMsg:
		m.currentStep = int(msg)
		m.stepStart = time.Now()

	case eventMsg:
		idx := msg.TenantIdx
		if idx >= 0 && idx < len(m.tenants) {
			t := &m.tenants[idx]
			t.events = append(t.events, Event(msg))
			if len(t.events) > 50 {
				t.events = t.events[len(t.events)-50:]
			}
			t.eventLog.SetContent(renderEventLog(t.events))
			t.eventLog.GotoBottom()
		}

	case quotaUpdateMsg:
		idx := msg.TenantIdx
		if idx >= 0 && idx < len(m.tenants) {
			m.tenants[idx].quotas = msg.Quotas
			for _, q := range msg.Quotas {
				if bar, ok := m.tenants[idx].progressBars[q.Resource]; ok {
					cmd := bar.SetPercent(q.Pct / 100.0)
					m.tenants[idx].progressBars[q.Resource] = bar
					cmds = append(cmds, cmd)
				}
			}
		}

	case clusterUpdateMsg:
		idx := msg.TenantIdx
		if idx >= 0 && idx < len(m.tenants) {
			m.tenants[idx].clusters = msg.Clusters
		}

	case clusterHeartbeatMsg:
		idx := msg.tenantIdx
		if idx >= 0 && idx < len(m.tenants) {
			for i, c := range m.tenants[idx].clusters {
				if c.Name == "Cluster "+msg.label {
					m.tenants[idx].clusters[i].Seq = msg.seq
					m.tenants[idx].clusters[i].AckSeq = msg.ackSeq
					m.tenants[idx].clusters[i].LastSeen = msg.lastSeen
				}
			}
		}

	case tbStatsUpdateMsg:
		idx := msg.TenantIdx
		if idx >= 0 && idx < len(m.tenants) {
			m.tenants[idx].tbStats = msg.Stats
		}

	case serverStatsMsg:
		m.serverStats = ServerStats(msg)
		cmds = append(cmds, serverStatsPollCmd(m.debugURL))
	}

	// Update progress bars for ALL tenants
	for i := range m.tenants {
		for k, bar := range m.tenants[i].progressBars {
			newBar, cmd := bar.Update(msg)
			if pb, ok := newBar.(progress.Model); ok {
				m.tenants[i].progressBars[k] = pb
			}
			cmds = append(cmds, cmd)
		}
		// Update event log viewport for ALL tenants
		newVP, cmd := m.tenants[i].eventLog.Update(msg)
		m.tenants[i].eventLog = newVP
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	// ── Header with tenant tabs
	elapsed := int(m.elapsed.Seconds())
	timeStr := fmt.Sprintf("[%02d:%02d]", elapsed/60, elapsed%60)

	// Build tenant tab labels (tenants + "Server Stats")
	tabParts := make([]string, len(m.tenants)+1)
	for i, t := range m.tenants {
		if i == m.selectedTenant {
			tabParts[i] = styleTabActive.Render("[" + t.name + "]")
		} else {
			tabParts[i] = styleTabInactive.Render(t.name)
		}
	}
	serverTabIdx := len(m.tenants)
	if m.selectedTenant == serverTabIdx {
		tabParts[serverTabIdx] = styleTabActive.Render("[Server Stats]")
	} else {
		tabParts[serverTabIdx] = styleTabInactive.Render("Server Stats")
	}
	tabStr := strings.Join(tabParts, "  ")
	tabHint := styleKeyHint.Render("[TAB] switch")

	header := styleHeader.Render(fmt.Sprintf(
		" BYOC Cloud Credit System — Live Demo   %s   %s   %s ",
		timeStr, tabStr, tabHint,
	))

	hints := styleKeyHint.Render("[SPACE] pause   [N] next step   [R] restart   [TAB] switch tenant   [Q] quit")

	// ── Server Stats tab
	if m.selectedTenant == serverTabIdx {
		body := renderServerStats(m, m.width-4)
		return lipgloss.JoinVertical(lipgloss.Left, header, body, hints)
	}

	// ── Left column: tenant + quota bars
	leftWidth := m.width/2 - 2
	leftCol := renderLeftColumn(m, leftWidth)

	// ── Right column: scenario timeline + event log + TB stats
	rightWidth := m.width - leftWidth - 4
	rightCol := renderRightColumn(m, rightWidth)

	// ── Bottom: cluster panels + key hints
	clusterRow := renderClusterRow(m)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		body,
		clusterRow,
		hints,
	)
}

// ─── Sub-views ────────────────────────────────────────────────────────────────

func renderLeftColumn(m Model, width int) string {
	t := m.tenants[m.selectedTenant]
	inner := width - 4 // border (2) + padding (2)

	titleStr := fmt.Sprintf("TENANT: %s (%s)", t.name, t.tier)
	lines := []string{
		styleTitle.Render(titleStr),
		styleDim.Render(fmt.Sprintf("Clusters: %d active", connectedCount(t.clusters))),
		"",
	}

	for _, q := range t.quotas {
		bar := t.progressBars[q.Resource]
		label := resourceLabel(q.Resource)
		status := q.Status
		usedStr := fmtNum(q.Used)
		limitStr := fmtNum(q.Limit)
		pctStr := fmt.Sprintf("%5.1f%%", q.Pct)

		// Color scheme by status
		var labelStyled, pctStyled, badge string
		switch status {
		case "exceeded":
			labelStyled = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render(label)
			pctStyled = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render(pctStr)
			badge = "  " + styleRed.Render("x HARD LIMIT")
		case "warning":
			labelStyled = styleYellow.Render(label)
			pctStyled = styleYellow.Render(pctStr)
			badge = "  " + styleYellow.Render("! warning")
		default:
			labelStyled = styleBold.Render(label)
			pctStyled = styleGreen.Render(pctStr)
			badge = ""
		}

		// Label row: "Label ............ X.X%" right-aligned
		labelW := lipgloss.Width(labelStyled)
		pctW := lipgloss.Width(pctStr) // use raw width for spacing calc
		gap := inner - labelW - pctW
		if gap < 1 {
			gap = 1
		}
		labelLine := labelStyled + strings.Repeat(" ", gap) + pctStyled

		// Bar row
		barLine := bar.View() + badge

		// Usage row
		usageLine := styleDim.Render(fmt.Sprintf("  used: %s / limit: %s", usedStr, limitStr))

		lines = append(lines, labelLine, barLine, usageLine, "")
	}

	content := strings.Join(lines, "\n")
	return styleBorder.Width(width).Render(content)
}

func renderRightColumn(m Model, width int) string {
	t := m.tenants[m.selectedTenant]

	// Scenario timeline
	timelineLines := []string{styleTitle.Render("SCENARIO TIMELINE"), ""}
	for i, s := range Scenario {
		var prefix string
		switch {
		case i < m.currentStep:
			prefix = styleGreen.Render("v")
		case i == m.currentStep:
			prefix = styleBlue.Render(">")
		default:
			prefix = styleDim.Render(" ")
		}
		timelineLines = append(timelineLines, fmt.Sprintf("  %s %d. %s", prefix, i+1, s.Name))
	}

	timeline := strings.Join(timelineLines, "\n")

	// Event log
	eventSection := styleTitle.Render("EVENT LOG") + "\n" + t.eventLog.View()

	// TB stats
	tbSection := renderTBStats(t.tbStats)

	content := lipgloss.JoinVertical(lipgloss.Left,
		timeline,
		"",
		eventSection,
		"",
		tbSection,
	)

	return styleBorder.Width(width).Render(content)
}

func renderClusterRow(m Model) string {
	t := m.tenants[m.selectedTenant]
	panels := make([]string, 0, len(t.clusters))
	for _, c := range t.clusters {
		var dot string
		if c.Connected {
			dot = styleGreen.Render("●")
		} else {
			dot = styleRed.Render("○")
		}
		lastStr := ""
		if !c.LastSeen.IsZero() {
			lastStr = c.LastSeen.Format("15:04:05")
		}
		panel := styleBorder.Render(fmt.Sprintf(
			" %s %s  %s %s\n stream: %s\n seq: %d  ack: %d\n rate: %s\n last: %s",
			dot, c.Name, styleDim.Render(c.Region), styleDim.Render(fmt.Sprintf("(%s)", c.Speed)),
			connectedStr(c.Connected),
			c.Seq, c.AckSeq,
			c.RateDesc,
			lastStr,
		))
		panels = append(panels, panel)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, panels...)
}

func renderTBStats(s TBStats) string {
	rejected := styleDim.Render(fmt.Sprintf("%d", s.Rejected))
	if s.Rejected > 0 {
		rejected = styleRed.Render(fmt.Sprintf("%d", s.Rejected))
	}
	idempotent := styleDim.Render(fmt.Sprintf("%d", s.Idempotent))
	if s.Idempotent > 0 {
		idempotent = styleBlue.Render(fmt.Sprintf("%d", s.Idempotent))
	}
	return styleTitle.Render("TB STATS") + "\n" +
		fmt.Sprintf("  Transfers submitted: %s\n", styleGreen.Render(fmt.Sprintf("%d", s.TransfersTotal))) +
		fmt.Sprintf("  Rejected (exceeds_credits): %s\n", rejected) +
		fmt.Sprintf("  Idempotent (exists): %s", idempotent)
}

func renderEventLog(events []Event) string {
	if len(events) == 0 {
		return styleDim.Render("  (waiting for events...)")
	}
	lines := make([]string, 0, len(events))
	for i := len(events) - 1; i >= 0 && i >= len(events)-20; i-- {
		e := events[i]
		ts := e.Timestamp.Format("15:04:05")
		var line string
		switch e.Kind {
		case EventHardLimit:
			line = styleRed.Render(fmt.Sprintf("  %s x %s", ts, e.Message))
		case EventSoftLimit:
			line = styleYellow.Render(fmt.Sprintf("  %s ! %s", ts, e.Message))
		case EventSurgePack:
			line = styleGreen.Render(fmt.Sprintf("  %s + %s", ts, e.Message))
		case EventDuplicate:
			line = styleBlue.Render(fmt.Sprintf("  %s ~ %s", ts, e.Message))
		case EventHeartbeatACK:
			line = styleGreen.Render(fmt.Sprintf("  %s > %s", ts, e.Message))
		default:
			line = styleDim.Render(fmt.Sprintf("  %s   %s", ts, e.Message))
		}
		lines = append([]string{line}, lines...)
	}
	return strings.Join(lines, "\n")
}

func renderServerStats(m Model, width int) string {
	s := m.serverStats

	statusLine := styleGreen.Render("● live")
	if !s.Ok {
		statusLine = styleRed.Render("○ unreachable (" + m.debugURL + ")")
	}

	gcPauseStr := fmt.Sprintf("%.2f ms", float64(s.GCPauseUS)/1000.0)

	uptimeStr := fmt.Sprintf("%ds", s.UptimeS)
	if s.UptimeS >= 60 {
		uptimeStr = fmt.Sprintf("%dm %ds", s.UptimeS/60, s.UptimeS%60)
	}

	// Aggregate TB stats across all tenants
	var totalTransfers, totalRejected, totalIdempotent int
	for _, t := range m.tenants {
		totalTransfers += t.tbStats.TransfersTotal
		totalRejected += t.tbStats.Rejected
		totalIdempotent += t.tbStats.Idempotent
	}

	rejStr := styleDim.Render(fmt.Sprintf("%d", totalRejected))
	if totalRejected > 0 {
		rejStr = styleRed.Render(fmt.Sprintf("%d", totalRejected))
	}
	idemStr := styleDim.Render(fmt.Sprintf("%d", totalIdempotent))
	if totalIdempotent > 0 {
		idemStr = styleBlue.Render(fmt.Sprintf("%d", totalIdempotent))
	}

	msgPerSecStr := styleDim.Render("—")
	if s.MsgPerSec > 0 {
		msgPerSecStr = styleGreen.Render(fmt.Sprintf("%.1f msg/s", s.MsgPerSec))
	}
	p50Str := styleDim.Render(fmt.Sprintf("%d µs", s.SignalP50Us))
	p99Str := styleDim.Render(fmt.Sprintf("%d µs", s.SignalP99Us))
	if s.SignalP99Us > 50_000 {
		p99Str = styleRed.Render(fmt.Sprintf("%d µs", s.SignalP99Us))
	} else if s.SignalP99Us > 10_000 {
		p99Str = styleYellow.Render(fmt.Sprintf("%d µs", s.SignalP99Us))
	}

	content := strings.Join([]string{
		styleTitle.Render("SERVER STATS") + "  " + statusLine,
		"",
		styleBold.Render("Throughput"),
		fmt.Sprintf("  Inbound:       %s  (total: %d)", msgPerSecStr, s.MsgTotal),
		fmt.Sprintf("  Active Streams:%s", styleGreen.Render(fmt.Sprintf("%d", s.ActiveStreams))),
		fmt.Sprintf("  signal p50:    %s", p50Str),
		fmt.Sprintf("  signal p99:    %s", p99Str),
		"",
		styleBold.Render("Go Runtime"),
		fmt.Sprintf("  Goroutines:    %s", styleGreen.Render(fmt.Sprintf("%d", s.Goroutines))),
		fmt.Sprintf("  Heap Alloc:    %s", styleGreen.Render(fmt.Sprintf("%.1f MB", s.HeapAllocMB))),
		fmt.Sprintf("  GC Runs:       %s  pause: %s", styleDim.Render(fmt.Sprintf("%d", s.GCRuns)), styleDim.Render(gcPauseStr)),
		fmt.Sprintf("  Uptime:        %s", styleDim.Render(uptimeStr)),
		"",
		styleBold.Render("TigerBeetle (all tenants)"),
		fmt.Sprintf("  Transfers submitted: %s", styleGreen.Render(fmt.Sprintf("%d", totalTransfers))),
		fmt.Sprintf("  Rejected (exceeds_credits): %s", rejStr),
		fmt.Sprintf("  Idempotent (exists): %s", idemStr),
		"",
		styleDim.Render("pprof: " + m.debugURL + "/debug/pprof/"),
	}, "\n")

	return styleBorder.Width(width).Render(content)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func connectedCount(clusters []ClusterState) int {
	n := 0
	for _, c := range clusters {
		if c.Connected {
			n++
		}
	}
	return n
}

func connectedStr(connected bool) string {
	if connected {
		return styleGreen.Render("connected")
	}
	return styleRed.Render("disconnected")
}

func resourceLabel(r string) string {
	switch r {
	case "cpu_hours":
		return "CPU Hours"
	case "memory_gb_hours":
		return "Memory GB-Hours"
	case "active_nodes":
		return "Active Nodes"
	default:
		return r
	}
}

func fmtNum(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
