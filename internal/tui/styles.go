package tui

import (
	"github.com/charmbracelet/lipgloss"

	"tuistream/internal/theme"
)

// centered horizontally centres every line of a block within width w. Each
// line is centred independently.
func centered(s string, w int) string {
	return lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(s)
}

// centeredFrame renders content inside a bordered card with every line centred
// within the card interior.
func centeredFrame(content string) string {
	return frameStyle.Align(lipgloss.Center).Render(content)
}

// centeredCard is centeredFrame's equivalent for the padded modal card style
// (confirms, pickers, progress splashes).
func centeredCard(content string) string {
	return cardStyle.Align(lipgloss.Center).Render(content)
}

// Colours and styles, (re)built from the active Omarchy theme by applyTheme.
// Mirrors omarchy-send's style block so the two sibling apps look related.
var (
	accent lipgloss.Color
	text   lipgloss.Color
	dim    lipgloss.Color
	muted  lipgloss.Color
	good   lipgloss.Color
	bad    lipgloss.Color

	titleBarStyle    lipgloss.Style
	tabActiveStyle   lipgloss.Style
	tabInactiveStyle lipgloss.Style
	frameStyle       lipgloss.Style
	cardStyle        lipgloss.Style
	footerStyle      lipgloss.Style
	titleStyle       lipgloss.Style
	headerStyle      lipgloss.Style
	labelStyle       lipgloss.Style
	valueStyle       lipgloss.Style

	roleSystemStyle    lipgloss.Style // boot/OS disk — off-limits
	roleAvailableStyle lipgloss.Style // ready to be picked
	roleInUseStyle     lipgloss.Style // already mounted somewhere
	devNameStyle       lipgloss.Style // /dev/... in inventory rows
)

func init() { applyTheme(theme.Default()) }

// cardWidth picks a consistent width for every card/frame the TUI draws.
// The cap (88) keeps content as a comfortably-centred column with balanced
// margins instead of filling a wide terminal edge-to-edge (which reads as
// left-bunched once the lines inside are left-aligned). The floor (40) keeps
// narrow terminals readable rather than broken. The -8 budget leaves at least
// a small margin on each side so the block always looks centred, never flush.
func cardWidth(termWidth int) int {
	w := termWidth - 8
	if w > 88 {
		w = 88
	}
	if w < 40 {
		w = 40
	}
	return w
}

// applyWidth rebuilds the width-sensitive style values for the current
// terminal width. Called once per render so all cards / frames have the
// same width, regardless of which renderer drew them.
func applyWidth(termWidth int) {
	w := cardWidth(termWidth)
	cardStyle = cardStyle.Width(w)
	frameStyle = frameStyle.Width(w)
}

func applyTheme(t theme.Theme) {
	accent = lipgloss.Color(t.Accent)
	text = lipgloss.Color(t.Fg)
	bg := lipgloss.Color(t.Bg)
	dim = lipgloss.Color(t.Dim)
	muted = lipgloss.Color(t.Muted)
	good = lipgloss.Color(t.Good)
	bad = lipgloss.Color(t.Bad)

	titleBarStyle = lipgloss.NewStyle().Bold(true).Foreground(bg).Background(accent)
	tabActiveStyle = lipgloss.NewStyle().Bold(true).Foreground(bg).Background(accent).Padding(0, 2)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(dim).Padding(0, 2)

	frameStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1)
	cardStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(1, 2)

	footerStyle = lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	headerStyle = lipgloss.NewStyle().Foreground(dim)
	labelStyle = lipgloss.NewStyle().Foreground(dim).Width(14)
	valueStyle = lipgloss.NewStyle().Foreground(text)

	roleSystemStyle = lipgloss.NewStyle().Foreground(bad).Bold(true)
	roleAvailableStyle = lipgloss.NewStyle().Foreground(good).Bold(true)
	roleInUseStyle = lipgloss.NewStyle().Foreground(muted)
	devNameStyle = lipgloss.NewStyle().Foreground(accent)
}
