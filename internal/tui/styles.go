package tui

import "github.com/charmbracelet/lipgloss"

// ANSI-indexed colors only (Projects/TUI-DESIGN.md): the TUI follows the terminal/Omarchy theme.
var (
	colBlack       = lipgloss.Color("0")
	colRed         = lipgloss.Color("1")
	colGreen       = lipgloss.Color("2")
	colYellow      = lipgloss.Color("3")
	colBlue        = lipgloss.Color("4")
	colMagenta     = lipgloss.Color("5")
	colCyan        = lipgloss.Color("6")
	colBrightBlack = lipgloss.Color("8")
	colBrightWhite = lipgloss.Color("15")
)

type styles struct {
	titleBar, titleFill                    lipgloss.Style
	badgeOK, badgeErr, badgeWarn, badgeAsk lipgloss.Style

	userLabel, userBody       lipgloss.Style
	agentLabel                lipgloss.Style
	toolName, toolArgs        lipgloss.Style
	toolErr, toolOK           lipgloss.Style
	riskWrite, riskDanger     lipgloss.Style
	notice, errorLine, dim    lipgloss.Style
	incident, incidentBody    lipgloss.Style
	diffAdd, diffDel, diffHdr lipgloss.Style

	inputBox lipgloss.Style

	help, helpKey lipgloss.Style

	dialogBox, dialogTitle, fieldLabel lipgloss.Style
}

func newStyles() styles {
	var s styles
	s.titleBar = lipgloss.NewStyle().Bold(true).Foreground(colBlack).Background(colBlue).Padding(0, 1)
	s.titleFill = lipgloss.NewStyle().Foreground(colBlack).Background(colBlue)
	badge := lipgloss.NewStyle().Bold(true).Foreground(colBlack).Padding(0, 1)
	s.badgeOK = badge.Background(colGreen)
	s.badgeErr = badge.Background(colRed)
	s.badgeWarn = badge.Background(colYellow)
	s.badgeAsk = badge.Background(colMagenta)

	s.userLabel = lipgloss.NewStyle().Bold(true).Foreground(colCyan)
	s.userBody = lipgloss.NewStyle().Foreground(colBrightWhite)
	s.agentLabel = lipgloss.NewStyle().Bold(true).Foreground(colBlue)
	s.toolName = lipgloss.NewStyle().Foreground(colCyan)
	s.toolArgs = lipgloss.NewStyle().Foreground(colBrightBlack)
	s.toolErr = lipgloss.NewStyle().Foreground(colRed)
	s.toolOK = lipgloss.NewStyle().Foreground(colGreen)
	s.riskWrite = lipgloss.NewStyle().Bold(true).Foreground(colYellow)
	s.riskDanger = lipgloss.NewStyle().Bold(true).Foreground(colRed)
	s.notice = lipgloss.NewStyle().Foreground(colYellow)
	s.errorLine = lipgloss.NewStyle().Bold(true).Foreground(colRed)
	s.dim = lipgloss.NewStyle().Foreground(colBrightBlack)
	s.incident = lipgloss.NewStyle().Bold(true).Foreground(colRed)
	s.incidentBody = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).
		BorderForeground(colRed).PaddingLeft(1)
	s.diffAdd = lipgloss.NewStyle().Foreground(colGreen)
	s.diffDel = lipgloss.NewStyle().Foreground(colRed)
	s.diffHdr = lipgloss.NewStyle().Foreground(colCyan)

	s.inputBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colCyan).Padding(0, 1)

	s.help = lipgloss.NewStyle().Foreground(colBrightBlack)
	s.helpKey = lipgloss.NewStyle().Foreground(colCyan)

	s.dialogBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colMagenta).Padding(1, 2)
	s.dialogTitle = lipgloss.NewStyle().Bold(true).Foreground(colMagenta)
	s.fieldLabel = lipgloss.NewStyle().Foreground(colBrightBlack)
	return s
}
