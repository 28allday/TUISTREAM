package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"tuistream/internal/firewall"
	"tuistream/internal/jellyfin"
	"tuistream/internal/step"
)

// setupStage is the current sub-state of the Setup tab. stageIdle is the
// default "inventory + key hints" view; other stages drive modal-like dialogs.
//
// The "running" and "done" states for an in-flight plan live on the root
// Model (see runStage / planRun) so both tabs can share the same step runner
// and progress rendering.
type setupStage int

const (
	stageIdle setupStage = iota
	stageConfirmInstall
	stageConfirmUninstall
	stageConfirmFirewall
	stageAddDrive
	stageImportPool          // import an existing detached btrfs pool (see importPoolState)
	stageMoveJellyfinPick    // choose which media drive to relocate onto (2+ managed)
	stageConfirmMoveJellyfin // last-chance confirm before migrating
)

// planRun tracks an in-flight multi-step plan (install / uninstall / add
// drive / firewall / copy / delete / rename). Owned by the root Model so
// either tab can start one and have it rendered consistently.
type planRun struct {
	title     string
	steps     []step.Step
	index     int
	err       error
	failedCmd string // string-form of the command that failed, for display
}

// runStage represents the lifecycle of the in-flight plan on the root model.
type runStage int

const (
	runIdle runStage = iota
	runRunning
	runDone
)

// --- messages ---

type statusLoadedMsg struct {
	status jellyfin.Status
	err    error
}

type stepFinishedMsg struct {
	err error
	cmd string // the command we tried to run (for error display)
}

type runCompleteMsg struct {
	title string
	err   error
}

// loadStatusCmd asynchronously refreshes Jellyfin's installed/active state.
func loadStatusCmd() tea.Cmd {
	return func() tea.Msg {
		s, err := jellyfin.LoadStatus()
		return statusLoadedMsg{status: s, err: err}
	}
}

// runStepCmd executes a single step in the background without ever leaving
// the TUI. stdout + stderr are tee'd to /tmp/tuistream/last-step.log so the
// error view can surface the tail on failure; while the command runs the
// model is in stageRunning and shows a spinner + step counter.
func runStepCmd(s step.Step) tea.Cmd {
	return func() tea.Msg {
		logPath := sessionLogPath()
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		// Truncate at the start of every step so the tail shown on error
		// only contains output from THIS step.
		logf, err := os.Create(logPath)
		if err == nil {
			s.Cmd.Stdout = logf
			s.Cmd.Stderr = logf
			defer logf.Close()
		}
		runErr := s.Cmd.Run()
		return stepFinishedMsg{err: runErr, cmd: s.Cmd.String()}
	}
}

// sessionLogPath chooses a path the operator can easily read without sudo:
// /tmp/tuistream/last-step.log on Linux (world-readable, survives until reboot),
// falling back to the user's cache if /tmp isn't usable. We deliberately don't
// use ~/.cache when running as root, because the operator who launched
// `sudo tuistream` then can't easily read /root/.cache.
func sessionLogPath() string {
	const tmpDir = "/tmp/tuistream"
	if err := os.MkdirAll(tmpDir, 0o1777); err == nil {
		_ = os.Chmod(tmpDir, 0o1777)
		return filepath.Join(tmpDir, "last-step.log")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", "tuistream", "last-step.log")
	}
	return "/tmp/tuistream-last-step.log"
}

// tail returns up to the last n lines of the session log, or an empty string.
func tailSessionLog(n int) string {
	b, err := os.ReadFile(sessionLogPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	// `script` records a "Script started" / "Script done" preamble — drop them.
	cleaned := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "Script started") || strings.HasPrefix(l, "Script done") {
			continue
		}
		cleaned = append(cleaned, l)
	}
	return strings.Join(cleaned, "\n")
}

// --- key handling on the Setup tab ---

// handleSetupKey routes a key press to the appropriate sub-stage handler.
// Returns the new model and any command. The bool result tells the caller
// whether the key was consumed (true) or should fall through to the root
// model's defaults (false).
//
// We pass the raw tea.KeyMsg through so sub-stages that contain a
// bubbles/textinput (like Add-drive's name entry) can feed it directly into
// the component; the simpler stages just look at msg.String().
func handleSetupKey(m Model, msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	key := msg.String()
	switch m.setup.stage {
	case stageIdle:
		return setupIdleKey(m, key)
	case stageConfirmInstall:
		return setupConfirmInstallKey(m, key)
	case stageConfirmUninstall:
		return setupConfirmUninstallKey(m, key)
	case stageConfirmFirewall:
		return setupConfirmFirewallKey(m, key)
	case stageAddDrive:
		return setupAddDriveKey(m, msg)
	case stageImportPool:
		return setupImportPoolKey(m, msg)
	case stageMoveJellyfinPick:
		return setupMoveJellyfinPickKey(m, key)
	case stageConfirmMoveJellyfin:
		return setupMoveJellyfinConfirmKey(m, key)
	}
	return m, nil, false
}

func setupIdleKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "d":
		// Toggle between the friendly drive list and the raw lsblk table.
		m.setup.showTech = !m.setup.showTech
		return m, nil, true
	case "i":
		// Install (or reinstall) — confirm before running.
		m.setup.stage = stageConfirmInstall
		m.setup.confirmIdx = 0 // default "Yes"
		return m, nil, true
	case "u":
		if m.status.IsInstalled() {
			m.setup.stage = stageConfirmUninstall
			m.setup.confirmIdx = 1 // default "No"
			return m, nil, true
		}
		m.flash = "Jellyfin isn't installed — nothing to uninstall."
		return m, nil, true
	case "a":
		if m.inventory == nil {
			m.flash = "Inventory still loading — press 'r' once it's done."
			return m, nil, true
		}
		cands := m.inventory.Candidates()
		m.setup.addDrive = newAddDriveState(cands)
		m.setup.stage = stageAddDrive
		return m, nil, true
	case "p":
		if m.inventory == nil {
			m.flash = "Inventory still loading — press 'r' once it's done."
			return m, nil, true
		}
		return startImportPool(m)
	case "f":
		m.setup.firewallTarget = !m.firewall.AllOpen()
		m.setup.stage = stageConfirmFirewall
		m.setup.confirmIdx = 0
		return m, nil, true
	case "j":
		return setupStartMoveJellyfin(m)
	case "y", "Y":
		// Copy the Jellyfin web address to the user's local clipboard (OSC 52),
		// the only thing that works from a headless box over SSH + tmux.
		if !m.status.ServiceActive {
			m.flash = "Start Jellyfin first — no web address to copy yet."
			return m, nil, true
		}
		urls := jellyfin.WebURLs()
		if len(urls) == 0 {
			m.flash = "Couldn't determine a web address to copy."
			return m, nil, true
		}
		m.flash = "Copied " + urls[0] + " to clipboard"
		return m, copyToClipboard(urls[0]), true
	}
	return m, nil, false
}

func setupConfirmFirewallKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.setup.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.setup.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.setup.confirmIdx = 0
		return runFirewall(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if m.setup.confirmIdx == 0 {
			return runFirewall(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runFirewall(m Model) (Model, tea.Cmd, bool) {
	var plan []step.Step
	title := ""
	if m.setup.firewallTarget {
		plan = firewall.OpenPlan()
		title = "Opening Jellyfin firewall ports (8096/tcp, 7359/udp)"
	} else {
		plan = firewall.ClosePlan()
		title = "Closing Jellyfin firewall ports (8096/tcp, 7359/udp)"
	}
	if len(plan) == 0 {
		m.flash = "Nothing to do (firewall already in target state?)"
		m.setup.stage = stageIdle
		return m, nil, true
	}
	m.setup.stage = stageIdle
	m.run = planRun{title: title, steps: plan, index: 0}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

// setupStartMoveJellyfin gates entry into the move-storage flow and routes to
// either the picker (2+ media drives) or straight to confirm (exactly one).
func setupStartMoveJellyfin(m Model) (Model, tea.Cmd, bool) {
	if !m.status.IsInstalled() {
		m.flash = "Install Jellyfin first (press i)."
		return m, nil, true
	}
	if jellyfin.LoadStorageState().Moved {
		m.flash = "Jellyfin storage is already on a media drive."
		return m, nil, true
	}
	if m.inventory == nil {
		m.flash = "Drives still loading — press r."
		return m, nil, true
	}
	managed := m.inventory.Managed()
	if len(managed) == 0 {
		m.flash = "Add a media drive first (press a)."
		return m, nil, true
	}
	m.setup.moveChoices = managed
	m.setup.movePickIdx = 0
	m.setup.confirmIdx = 0
	if len(managed) == 1 {
		m.setup.moveTarget = managed[0]
		m.setup.stage = stageConfirmMoveJellyfin
		return m, nil, true
	}
	m.setup.stage = stageMoveJellyfinPick
	return m, nil, true
}

func setupMoveJellyfinPickKey(m Model, key string) (Model, tea.Cmd, bool) {
	st := &m.setup
	switch key {
	case "up", "k":
		if st.movePickIdx > 0 {
			st.movePickIdx--
		}
		return m, nil, true
	case "down", "j":
		if st.movePickIdx < len(st.moveChoices)-1 {
			st.movePickIdx++
		}
		return m, nil, true
	case "esc":
		st.stage = stageIdle
		return m, nil, true
	case "enter":
		st.moveTarget = st.moveChoices[st.movePickIdx]
		st.confirmIdx = 0
		st.stage = stageConfirmMoveJellyfin
		return m, nil, true
	}
	return m, nil, false
}

func setupMoveJellyfinConfirmKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.setup.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.setup.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.setup.confirmIdx = 0
		return runMoveJellyfin(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if m.setup.confirmIdx == 0 {
			return runMoveJellyfin(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runMoveJellyfin(m Model) (Model, tea.Cmd, bool) {
	plan := jellyfin.MovePlan(jellyfin.MoveOptions{
		MountPoint:  m.setup.moveTarget.MountPoint,
		ServiceUnit: m.status.ServiceUnit,
	})
	m.setup.stage = stageIdle
	m.run = planRun{
		title: "Moving Jellyfin storage to " + m.setup.moveTarget.MountPoint,
		steps: plan,
		index: 0,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

func setupConfirmInstallKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.setup.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.setup.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.setup.confirmIdx = 0
		return runInstall(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if m.setup.confirmIdx == 0 {
			return runInstall(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runInstall(m Model) (Model, tea.Cmd, bool) {
	plan := jellyfin.InstallPlan()
	m.setup.stage = stageIdle
	m.run = planRun{
		title: "Installing Jellyfin",
		steps: plan,
		index: 0,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

func setupConfirmUninstallKey(m Model, key string) (Model, tea.Cmd, bool) {
	switch key {
	case "left", "h":
		m.setup.confirmIdx = 0
		return m, nil, true
	case "right", "l":
		m.setup.confirmIdx = 1
		return m, nil, true
	case "y", "Y":
		m.setup.confirmIdx = 0
		return runUninstall(m)
	case "n", "N", "esc":
		m.setup.stage = stageIdle
		return m, nil, true
	case "enter":
		if m.setup.confirmIdx == 0 {
			return runUninstall(m)
		}
		m.setup.stage = stageIdle
		return m, nil, true
	}
	return m, nil, false
}

func runUninstall(m Model) (Model, tea.Cmd, bool) {
	plan := jellyfin.UninstallPlan(m.status.Packages, false /* don't purge data by default */)
	if len(plan) == 0 {
		m.flash = "Nothing to uninstall."
		m.setup.stage = stageIdle
		return m, nil, true
	}
	m.setup.stage = stageIdle
	m.run = planRun{
		title: "Uninstalling Jellyfin",
		steps: plan,
		index: 0,
	}
	m.runStage = runRunning
	m.runOwnerTab = tabSetup
	return m, runStepCmd(plan[0]), true
}

// --- step-finished routing ---

// advanceRun handles a stepFinishedMsg by either firing the next step or
// finishing the run. Returns the updated model + the next command.
func advanceRun(m Model, msg stepFinishedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.run.err = msg.err
		m.run.failedCmd = msg.cmd
		m.runStage = runDone
		return m, nil
	}
	m.run.index++
	if m.run.index >= len(m.run.steps) {
		m.runStage = runDone
		return m, tea.Batch(loadStatusCmd(), loadFirewallCmd(), loadInventoryCmd(m.username))
	}
	next := m.run.steps[m.run.index]
	return m, runStepCmd(next)
}

// dismissRun resets the in-flight plan and returns the owning tab to its
// idle state. Called when the user presses any key on the runDone splash.
func dismissRun(m Model) (Model, tea.Cmd) {
	m.run = planRun{}
	m.runStage = runIdle
	switch m.runOwnerTab {
	case tabSetup:
		m.setup.stage = stageIdle
	case tabManage:
		m.manage.stage = manageIdle
		m.manage.active = manageAction{}
	}
	return m, tea.Batch(loadStatusCmd(), loadInventoryCmd(m.username))
}

// --- rendering for the stages ---

// renderSetupActionBar shows the keyboard shortcuts at the bottom of the
// idle setup view. The set of keys depends on current install + firewall state.
func renderSetupActionBar(installed, fwOpen, serviceActive, jellyfinMoved, hasDetachedPool bool, width int) string {
	var keys []string
	if installed {
		keys = append(keys, "[i] reinstall")
		keys = append(keys, "[u] uninstall")
	} else {
		keys = append(keys, "[i] install Jellyfin")
	}
	keys = append(keys, "[a] add drive")
	if hasDetachedPool {
		keys = append(keys, "[p] import pool")
	}
	if fwOpen {
		keys = append(keys, "[f] close firewall")
	} else {
		keys = append(keys, "[f] open firewall")
	}
	if installed {
		if jellyfinMoved {
			keys = append(keys, headerStyle.Render("[j] storage on media ✓"))
		} else {
			keys = append(keys, "[j] move Jellyfin storage")
		}
	}
	if serviceActive {
		keys = append(keys, "[y] copy URL")
	}
	return wrapKeyBar(keys, width)
}

// renderFirewallLine shows the UFW status in the Setup tab header.
func renderFirewallLine(s firewall.State) string {
	if !s.UFWInstalled {
		return labelStyle.Render("Firewall:") + " " +
			headerStyle.Render("UFW not installed (no firewall management)")
	}
	switch {
	case s.AllOpen():
		return labelStyle.Render("Firewall:") + " " +
			roleAvailableStyle.Render("8096/tcp + 7359/udp open")
	case s.AnyOpen():
		mix := []string{}
		if s.WebOpen {
			mix = append(mix, roleAvailableStyle.Render("8096/tcp open"))
		} else {
			mix = append(mix, roleSystemStyle.Render("8096/tcp closed"))
		}
		if s.DiscoveryOpen {
			mix = append(mix, roleAvailableStyle.Render("7359/udp open"))
		} else {
			mix = append(mix, roleSystemStyle.Render("7359/udp closed"))
		}
		return labelStyle.Render("Firewall:") + " " + strings.Join(mix, " · ")
	default:
		return labelStyle.Render("Firewall:") + " " +
			roleSystemStyle.Render("closed (LAN devices can't reach Jellyfin)")
	}
}

// renderConfirmFirewall shows the open/close confirmation modal.
func renderConfirmFirewall(s firewall.State, opening bool, idx int) string {
	var rows []string
	verb := "Open"
	desc := "Allow other devices on your LAN to reach Jellyfin's web UI and find this server in their apps."
	if !opening {
		verb = "Close"
		desc = "Block LAN traffic to Jellyfin. Localhost access still works, but the apps on your phone / TV won't be able to reach this server."
	}
	rows = append(rows, titleStyle.Render(verb+" Jellyfin firewall ports?"))
	rows = append(rows, "")
	rows = append(rows, desc)
	rows = append(rows, "")
	rows = append(rows, "Ports:")
	rows = append(rows, "  · "+devNameStyle.Render(firewall.WebPort)+"   (HTTP web UI / API)")
	rows = append(rows, "  · "+devNameStyle.Render(firewall.DiscoveryPort)+"   (Jellyfin client auto-discovery)")
	if !s.UFWInstalled {
		rows = append(rows, "")
		rows = append(rows, roleSystemStyle.Render("UFW isn't installed — nothing to do."))
	}
	rows = append(rows, "")
	yes := "  Yes  "
	no := "  Cancel  "
	if idx == 0 {
		yes = roleAvailableStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleSystemStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y / n shortcut · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

// renderStatusLine shows "Jellyfin: not installed" or "Jellyfin: installed
// (jellyfin-bin) · service active" at the top of the Setup tab.
func renderStatusLine(s jellyfin.Status) string {
	if !s.IsInstalled() {
		return labelStyle.Render("Jellyfin:") + " " +
			roleSystemStyle.Render("not installed")
	}
	bits := []string{
		labelStyle.Render("Jellyfin:"),
		roleAvailableStyle.Render("installed"),
	}
	if s.ServiceActive {
		bits = append(bits, roleAvailableStyle.Render("· service active"))
	} else if s.ServiceUnit != "" {
		bits = append(bits, roleInUseStyle.Render("· service stopped"))
	}
	line := strings.Join(bits, " ")

	// Once the service is up, show where to point a browser — the whole point
	// of a headless server. Listed for hostname.local + each LAN/Tailscale IP.
	if s.ServiceActive {
		if urls := jellyfin.WebURLs(); len(urls) > 0 {
			styled := make([]string, len(urls))
			for i, u := range urls {
				styled[i] = devNameStyle.Render(u)
			}
			line += "\n" + labelStyle.Render("Browse:") + " " +
				strings.Join(styled, headerStyle.Render("  ·  "))
		}
	}
	return line
}

func sprintList(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s, ", ")
}

func renderConfirmInstall(idx int) string {
	var rows []string
	rows = append(rows, titleStyle.Render("Install Jellyfin?"))
	rows = append(rows, "")
	rows = append(rows, "This will install from the official Arch extra repo:")
	rows = append(rows, "  · "+roleAvailableStyle.Render("jellyfin-server"))
	rows = append(rows, "  · "+roleAvailableStyle.Render("jellyfin-web"))
	rows = append(rows, "  · "+roleAvailableStyle.Render("jellyfin-ffmpeg"))
	rows = append(rows, "")
	rows = append(rows, "Then enable & start "+devNameStyle.Render("jellyfin.service")+".")
	rows = append(rows, "")
	yes := "  Yes, install  "
	no := "  Cancel  "
	if idx == 0 {
		yes = roleAvailableStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleSystemStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y / n shortcut · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderConfirmUninstall(s jellyfin.Status, idx int) string {
	packages := s.Packages.Installed()
	var rows []string
	rows = append(rows, titleStyle.Render("Uninstall Jellyfin?"))
	rows = append(rows, "")
	rows = append(rows, "This will:")
	rows = append(rows, "  · Stop and disable jellyfin.service")
	rows = append(rows, "  · Remove "+roleSystemStyle.Render(sprintList(packages)))
	rows = append(rows, "")
	rows = append(rows, "Your library DB and settings in /var/lib/jellyfin will be KEPT.")
	rows = append(rows, "")
	yes := "  Yes, uninstall  "
	no := "  Cancel  "
	if idx == 0 {
		yes = roleSystemStyle.Render("▸" + yes)
		no = "  " + no
	} else {
		yes = "  " + yes
		no = roleAvailableStyle.Render("▸" + no)
	}
	rows = append(rows, yes+"   "+no)
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("←/→ move · enter confirm · y / n shortcut · esc cancel"))
	return centeredCard(strings.Join(rows, "\n"))
}

// renderRunning is the in-flight progress view: title, step counter, a
// spinner against the current step's name, and the completed steps so far.
// No terminal-switching, no flicker — the TUI stays put while pacman /
// mkfs / mount / setfacl / ufw run in the background.
func renderRunning(sp spinner.Model, run planRun) string {
	var rows []string
	rows = append(rows, titleStyle.Render(run.title))
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(fmt.Sprintf(
		"Step %d of %d", run.index+1, len(run.steps))))
	rows = append(rows, "")
	for i, s := range run.steps {
		switch {
		case i < run.index:
			rows = append(rows, "  "+roleAvailableStyle.Render("✓")+"  "+s.Title)
		case i == run.index:
			rows = append(rows, "  "+sp.View()+" "+roleAvailableStyle.Render(s.Title))
		default:
			rows = append(rows, "  "+headerStyle.Render("·  "+s.Title))
		}
	}
	rows = append(rows, "")
	rows = append(rows, headerStyle.Render(
		"Captured output is being written to "+sessionLogPath()+
			" — tail it from another shell if you want a live view."))
	return centeredCard(strings.Join(rows, "\n"))
}

func renderRunDone(run planRun) string {
	var rows []string
	if run.err != nil {
		rows = append(rows, roleSystemStyle.Render("Something went wrong."))
		rows = append(rows, "")
		rows = append(rows, headerStyle.Render(run.title))
		rows = append(rows, "")
		failedStep := ""
		if run.index < len(run.steps) {
			failedStep = run.steps[run.index].Title
		}
		if failedStep != "" {
			rows = append(rows, "Failed step:  "+failedStep)
		}
		if run.failedCmd != "" {
			rows = append(rows, "Command:      "+run.failedCmd)
		}
		rows = append(rows, "Error:        "+run.err.Error())
		rows = append(rows, "")
		if tail := tailSessionLog(20); tail != "" {
			rows = append(rows, headerStyle.Render("Last output (from "+sessionLogPath()+"):"))
			rows = append(rows, frameStyle.Render(tail))
		} else {
			rows = append(rows, headerStyle.Render(
				"No captured output. Re-run the command manually to see what happened:"))
			if run.failedCmd != "" {
				rows = append(rows, "  "+run.failedCmd)
			}
		}
	} else {
		rows = append(rows, roleAvailableStyle.Render("✓ "+run.title+" — complete."))
		rows = append(rows, "")
		for _, s := range run.steps {
			rows = append(rows, "  ✓ "+s.Title)
		}
	}
	rows = append(rows, "")
	rows = append(rows, footerStyle.Render("Press any key to return."))
	return centeredCard(strings.Join(rows, "\n"))
}
