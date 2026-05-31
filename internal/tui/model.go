// Package tui implements the Bubble Tea front end. Two tabs: Setup and
// Manage. Tab/Shift-Tab to switch; q to quit; '?' for help.
package tui

import (
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tuistream/internal/drives"
	"tuistream/internal/firewall"
	"tuistream/internal/jellyfin"
	"tuistream/internal/theme"
)

type tab int

const (
	tabSetup tab = iota
	tabManage
	tabMonitor

	tabCount = 3
)

func (t tab) String() string {
	switch t {
	case tabSetup:
		return "Setup"
	case tabManage:
		return "Manage"
	case tabMonitor:
		return "Monitor"
	}
	return "?"
}

// Model is the root model. Each tab's state lives in its own field so the
// tab switch is just a number bump and we never lose work-in-progress.
type Model struct {
	width, height int

	currentTab tab
	username   string // owning Linux user (SUDO_USER if available)

	inventory    *drives.Inventory
	inventoryErr error

	status    jellyfin.Status
	statusErr error

	firewall firewall.State

	setup   setupModel
	manage  manageModel
	monitor monitorModel

	// In-flight plan, shared between tabs. runOwnerTab is the tab the
	// run started from — we return to it when the user dismisses the
	// "done" splash.
	run         planRun
	runStage    runStage
	runOwnerTab tab

	// spinner ticked while a step is running in the background.
	spinner spinner.Model

	// flash message shown in the footer for a few seconds after an action
	flash string
}

// NewModel constructs a model with the given theme. The inventory is loaded
// lazily on first View() so startup is instant even on slow lsblk hosts.
func NewModel(t theme.Theme) Model {
	applyTheme(t)
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent).Bold(true)
	return Model{
		currentTab: tabSetup,
		username:   detectUser(),
		setup:      newSetupModel(),
		manage:     newManageModel(),
		monitor:    newMonitorModel(),
		spinner:    sp,
	}
}

// Init is the Bubble Tea entry point. We kick off the first inventory load,
// Jellyfin status check, and firewall snapshot asynchronously so the UI
// paints immediately. The monitor tick fires on a 5s interval to refresh
// the Monitor tab's health snapshot.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadInventoryCmd(m.username),
		loadStatusCmd(),
		loadFirewallCmd(),
		loadHealthCmd(m.inventory),
		monitorTickCmd(),
		cpuSampleCmd(nil), // seed the previous-sample slot
		cpuTickCmd(),
		m.spinner.Tick,
	)
}

type firewallLoadedMsg struct{ state firewall.State }

func loadFirewallCmd() tea.Cmd {
	return func() tea.Msg {
		return firewallLoadedMsg{state: firewall.LoadState()}
	}
}

type inventoryLoadedMsg struct {
	inv *drives.Inventory
	err error
}

func loadInventoryCmd(username string) tea.Cmd {
	return func() tea.Msg {
		inv, err := drives.Load(username)
		return inventoryLoadedMsg{inv: inv, err: err}
	}
}

// Update routes messages. Global keys (quit, tab switch) are handled here;
// tab-specific keys (Setup actions etc.) are delegated to per-tab handlers.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case inventoryLoadedMsg:
		m.inventory = msg.inv
		m.inventoryErr = msg.err
		return m, nil

	case statusLoadedMsg:
		m.status = msg.status
		m.statusErr = msg.err
		return m, nil

	case firewallLoadedMsg:
		m.firewall = msg.state
		return m, nil

	case healthLoadedMsg:
		m.monitor.snap = msg.snap
		m.monitor.err = msg.err
		return m, nil

	case monitorTickMsg:
		// Refresh on tick. Always re-arm so the next tick fires.
		return m, tea.Batch(loadHealthCmd(m.inventory), monitorTickCmd())

	case cpuTickMsg:
		return m, tea.Batch(cpuSampleCmd(m.monitor.cpuPrev), cpuTickCmd())

	case cpuSampleMsg:
		m.monitor.cpuPrev = msg.sample
		if len(msg.usage.Cores) == 0 {
			return m, nil
		}
		m.monitor.cpuUsage = msg.usage
		if m.monitor.cpuHist == nil || len(m.monitor.cpuHist) != len(msg.usage.Cores) {
			m.monitor.cpuHist = make([][]float64, len(msg.usage.Cores))
		}
		for i, v := range msg.usage.Cores {
			m.monitor.cpuHist[i] = append(m.monitor.cpuHist[i], v)
			if len(m.monitor.cpuHist[i]) > cpuHistoryLen {
				m.monitor.cpuHist[i] = m.monitor.cpuHist[i][len(m.monitor.cpuHist[i])-cpuHistoryLen:]
			}
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case stepFinishedMsg:
		var cmd tea.Cmd
		m, cmd = advanceRun(m, msg)
		return m, cmd

	case tea.KeyMsg:
		key := msg.String()

		// While a plan is running, only allow ctrl+c — nothing else should
		// fire and the user can't switch tabs out from under it.
		if m.runStage == runRunning {
			if key == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}

		// Any key dismisses the runDone splash and returns to the owning
		// tab's idle state.
		if m.runStage == runDone {
			nm, cmd := dismissRun(m)
			return nm, cmd
		}

		// Stage-active keys: when a modal is up on the Setup tab, route through
		// its handlers FIRST so e.g. 'q' inside an input still works.
		if m.currentTab == tabSetup && m.setup.stage != stageIdle {
			nm, cmd, consumed := handleSetupKey(m, msg)
			if consumed {
				return nm, cmd
			}
		}
		// Same for Manage tab when it has a modal open.
		if m.currentTab == tabManage && m.manage.stage != manageIdle {
			nm, cmd, consumed := handleManageKey(m, msg)
			if consumed {
				return nm, cmd
			}
		}

		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab", "right":
			m.currentTab = (m.currentTab + 1) % tabCount
			return m, nil
		case "shift+tab", "left":
			m.currentTab = (m.currentTab + tabCount - 1) % tabCount
			return m, nil
		case "r":
			m.flash = "Refreshing…"
			return m, tea.Batch(loadInventoryCmd(m.username), loadStatusCmd(), loadFirewallCmd())
		}

		// Idle Setup-tab keys ('i' install, 'u' uninstall, 'a' add drive, 'f' firewall)
		if m.currentTab == tabSetup {
			if nm, cmd, consumed := handleSetupKey(m, msg); consumed {
				return nm, cmd
			}
		}
		// Idle Manage-tab keys ('c' copy, 'd' delete, 'n' rename)
		if m.currentTab == tabManage {
			if nm, cmd, consumed := handleManageKey(m, msg); consumed {
				return nm, cmd
			}
		}
	}
	return m, nil
}

// View composes the chrome (title bar, tab bar, footer) around the active
// tab's view.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	// Re-stretch cards / frames to the current terminal width so every
	// element drawn this frame lines up regardless of which renderer
	// produced it.
	applyWidth(m.width)

	// Title bar is naturally full-width — centre its text content.
	title := titleBarStyle.Width(m.width).Align(lipgloss.Center).Render(
		"TUISTREAM  —  headless Jellyfin for Omarchy / Arch",
	)

	tabs := []string{}
	for i := 0; i < tabCount; i++ {
		t := tab(i)
		style := tabInactiveStyle
		if t == m.currentTab {
			style = tabActiveStyle
		}
		tabs = append(tabs, style.Render(t.String()))
	}
	tabBar := lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(
		lipgloss.JoinHorizontal(lipgloss.Top, tabs...),
	)

	body := ""
	switch {
	case m.runStage == runRunning:
		body = renderRunning(m.spinner, m.run)
	case m.runStage == runDone:
		body = renderRunDone(m.run)
	case m.currentTab == tabSetup:
		body = m.setup.view(m)
	case m.currentTab == tabManage:
		body = m.manage.view(m)
	case m.currentTab == tabMonitor:
		body = m.monitor.view(m)
	}

	footer := m.renderFooter()

	// Centre the body horizontally inside the terminal. Cards are sized
	// via cardWidth(m.width); this just slides them to the middle.
	chromeHeight := lipgloss.Height(title) + lipgloss.Height(tabBar) + lipgloss.Height(footer)
	bodyHeight := m.height - chromeHeight
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	bodyBlock := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Align(lipgloss.Center).
		Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, title, tabBar, bodyBlock, footer)
}

func (m Model) renderFooter() string {
	hints := []string{
		"tab/⇆ switch", "r refresh", "q quit",
	}
	if m.flash != "" {
		return footerStyle.Width(m.width).Align(lipgloss.Center).Render(m.flash + "   ·   " + strings.Join(hints, "  ·  "))
	}
	return footerStyle.Width(m.width).Align(lipgloss.Center).Render(strings.Join(hints, "  ·  "))
}

// detectUser returns the user we should operate on behalf of: SUDO_USER if
// present (the real user behind a sudo invocation), otherwise the current
// user. Empty string if neither resolves.
func detectUser() string {
	if su := envSudoUser(); su != "" {
		return su
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func envSudoUser() string {
	if v := os.Getenv("SUDO_USER"); v != "" && v != "root" {
		return v
	}
	return ""
}

// WithInventory injects an already-loaded inventory and returns the updated
// model. Useful for tests.
func (m Model) WithInventory(inv *drives.Inventory) Model {
	m.inventory = inv
	m.inventoryErr = nil
	return m
}

// WithTab switches the active tab and returns the updated model. Tests only.
func (m Model) WithTab(i int) Model {
	m.currentTab = tab(i % tabCount)
	return m
}

// summaryLine renders the "n drives detected, m available" line used in both
// tabs to keep the user oriented after a refresh.
func summaryLine(inv *drives.Inventory) string {
	if inv == nil {
		return headerStyle.Render("Loading drives…")
	}
	var disks, parts, available, managed int
	for _, d := range inv.All {
		switch d.Type {
		case "disk":
			disks++
		case "part", "crypt":
			parts++
		}
		switch d.Role {
		case drives.RoleAvailable:
			available++
		case drives.RoleManagedOurs:
			managed++
		}
	}
	msg := fmt.Sprintf(
		"%d disks · %d partitions · %d available · %d already managed",
		disks, parts, available, managed,
	)
	return headerStyle.Render(msg)
}
