// Package tui is the Bubble Tea client: a chat transcript with live tool calls, and an approval dialog for
// changes requested by any session (including incidents and Telegram).
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/server"
)

// GlamourStyle is resolved in main before Bubble Tea owns the terminal ("dark" or "light").
var GlamourStyle = "dark"

type keyMap struct {
	Send, Cancel, Approvals, Approve, Deny, Close, Up, Down, PageUp, PageDown, Quit key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Send:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
		Cancel:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel run")),
		Approvals: key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "approvals")),
		Approve:   key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "approve")),
		Deny:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "deny")),
		Close:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "later")),
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll")),
		PageUp:    key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "scroll up")),
		PageDown:  key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn", "scroll down")),
		Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

// chatHelp and dialogHelp implement help.KeyMap for the two modes.
type chatHelp struct{ k keyMap }

func (h chatHelp) ShortHelp() []key.Binding {
	return []key.Binding{h.k.Send, h.k.Cancel, h.k.Approvals, h.k.PageUp, h.k.Quit}
}
func (h chatHelp) FullHelp() [][]key.Binding { return [][]key.Binding{h.ShortHelp()} }

type dialogHelp struct{ k keyMap }

func (h dialogHelp) ShortHelp() []key.Binding {
	return []key.Binding{h.k.Approve, h.k.Deny, h.k.Up, h.k.Close}
}
func (h dialogHelp) FullHelp() [][]key.Binding { return [][]key.Binding{h.ShortHelp()} }

type entryKind int

const (
	kUser entryKind = iota
	kAnswer
	kTool
	kNotice
	kError
	kIncident
)

type entry struct {
	kind entryKind
	text string
	ev   events.Event

	// rendered markdown cache (answers, diagnoses)
	md  string
	mdW int
}

type Model struct {
	be      server.Backend
	session string
	st      styles
	keys    keyMap
	help    help.Model

	width, height int
	vp            viewport.Model
	input         textinput.Model
	spin          spinner.Model

	entries   []entry
	renderer  *glamour.TermRenderer
	rendW     int
	busy      bool
	connected bool
	info      string
	model     string
	status    string

	approvals   []events.Event // pending, oldest first
	showDialog  bool
	dialogVP    viewport.Model
	dialogLines int // lines of the current preview
}

func New(be server.Backend, session string) Model {
	in := textinput.New()
	in.Placeholder = "Ask about the cluster…  (/help)"
	in.Prompt = "› "
	in.Focus()
	in.CharLimit = 4000
	vp := viewport.New(80, 20)
	vp.KeyMap = viewport.KeyMap{} // scrolling is handled explicitly so typing never scrolls
	dvp := viewport.New(60, 10)
	dvp.KeyMap = viewport.KeyMap{}
	st := newStyles()
	h := help.New()
	h.Styles.ShortKey, h.Styles.ShortDesc, h.Styles.ShortSeparator = st.helpKey, st.help, st.help
	return Model{
		be: be, session: session, st: st, keys: defaultKeyMap(), help: h,
		vp: vp, input: in, dialogVP: dvp, connected: true,
		spin: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(colYellow))),
	}
}

type evMsg events.Event
type closedMsg struct{}

func (m Model) wait() tea.Cmd {
	return func() tea.Msg {
		e, ok := <-m.be.Events()
		if !ok {
			return closedMsg{}
		}
		return evMsg(e)
	}
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.wait(), textinput.Blink, m.spin.Tick) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refresh(true)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
		if m.busy {
			m.refresh(m.vp.AtBottom()) // animate the spinner of the running tool call
		}
	case closedMsg:
		m.connected = false
		m.status = "connection closed"
		return m, nil
	case evMsg:
		m.onEvent(events.Event(msg))
		cmds = append(cmds, m.wait())
	case tea.KeyMsg:
		if key.Matches(msg, m.keys.Quit) {
			return m, tea.Quit
		}
		if m.showDialog && len(m.approvals) > 0 {
			return m, m.dialogKey(msg)
		}
		switch {
		case key.Matches(msg, m.keys.Send):
			cmds = append(cmds, m.submit())
		case key.Matches(msg, m.keys.Cancel):
			if m.busy {
				m.send(server.ClientMsg{Type: "cancel"})
			}
		case key.Matches(msg, m.keys.Approvals):
			if len(m.approvals) > 0 {
				m.openDialog()
			} else {
				m.status = "no pending approvals"
			}
		case key.Matches(msg, m.keys.PageUp):
			m.vp.HalfPageUp()
		case key.Matches(msg, m.keys.PageDown):
			m.vp.HalfPageDown()
		case msg.Type == tea.KeyUp:
			m.vp.ScrollUp(1)
		case msg.Type == tea.KeyDown:
			m.vp.ScrollDown(1)
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) dialogKey(msg tea.KeyMsg) tea.Cmd {
	cur := m.approvals[0]
	switch {
	case key.Matches(msg, m.keys.Approve):
		m.send(server.ClientMsg{Type: "approve", ID: cur.ID, OK: true})
	case key.Matches(msg, m.keys.Deny):
		m.send(server.ClientMsg{Type: "approve", ID: cur.ID, OK: false, Reason: "denied in TUI"})
	case key.Matches(msg, m.keys.Close):
		m.showDialog = false
	case key.Matches(msg, m.keys.Up):
		m.dialogVP.ScrollUp(1)
	case key.Matches(msg, m.keys.Down):
		m.dialogVP.ScrollDown(1)
	case key.Matches(msg, m.keys.PageUp):
		m.dialogVP.HalfPageUp()
	case key.Matches(msg, m.keys.PageDown):
		m.dialogVP.HalfPageDown()
	}
	return nil
}

func (m *Model) send(msg server.ClientMsg) {
	if err := m.be.Send(msg); err != nil {
		m.status = "send failed: " + err.Error()
	}
}

func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.SetValue("")
	switch strings.Fields(text)[0] {
	case "/quit", "/exit":
		return tea.Quit
	case "/reset":
		m.entries = nil
		m.send(server.ClientMsg{Type: "reset"})
	case "/cancel":
		m.send(server.ClientMsg{Type: "cancel"})
	case "/audit":
		m.send(server.ClientMsg{Type: "audit"})
	case "/changes":
		m.send(server.ClientMsg{Type: "audit", Changes: true})
	case "/clear":
		m.entries = nil
	case "/help":
		m.add(entry{kind: kNotice, text: "commands: /reset (new conversation) · /cancel · /audit (log) · /changes " +
			"(approved/denied changes) · /clear (screen only) · /quit\nkeys: esc cancels a running question · " +
			"ctrl+a opens pending approvals · pgup/pgdn scroll"})
	default:
		if strings.HasPrefix(text, "/") {
			m.status = "unknown command " + text
			return nil
		}
		if m.busy {
			m.status = "still working — esc cancels"
			m.input.SetValue(text)
			return nil
		}
		m.send(server.ClientMsg{Type: "ask", Text: text})
	}
	m.refresh(true)
	return nil
}

func (m *Model) onEvent(e events.Event) {
	follow := m.vp.AtBottom()
	switch e.Type {
	case events.Hello:
		m.connected, m.info, m.busy = true, e.Text, e.OK
		m.status = "connected"
	case events.User:
		m.add(entry{kind: kUser, text: e.Text, ev: e})
	case events.Busy:
		m.busy = true
	case events.Idle:
		m.busy = false
	case events.Model:
		m.model = e.Text
	case events.ToolCall:
		m.add(entry{kind: kTool, ev: e})
	case events.ToolResult:
		// Attach the result to the latest matching call.
		for i := len(m.entries) - 1; i >= 0; i-- {
			if en := &m.entries[i]; en.kind == kTool && en.ev.Tool == e.Tool && en.text == "" {
				en.text = "done"
				if e.Error != "" {
					en.text = "error: " + e.Error
				}
				break
			}
		}
	case events.Answer:
		m.add(entry{kind: kAnswer, text: e.Text})
	case events.Error:
		m.add(entry{kind: kError, text: e.Error})
	case events.Notice:
		if e.Error == "disconnected" {
			m.connected = false
		}
		m.add(entry{kind: kNotice, text: e.Text})
	case events.Incident:
		m.add(entry{kind: kIncident, text: e.Text, ev: e})
	case events.Approval:
		for _, a := range m.approvals {
			if a.ID == e.ID {
				return
			}
		}
		m.approvals = append(m.approvals, e)
		if len(m.approvals) == 1 {
			m.openDialog()
		}
	case events.ApprovalResolved:
		for i, a := range m.approvals {
			if a.ID == e.ID {
				m.approvals = append(m.approvals[:i], m.approvals[i+1:]...)
				break
			}
		}
		verdict := "approved"
		if !e.OK {
			verdict = "denied"
		}
		txt := fmt.Sprintf("%s %s by %s", e.Tool, verdict, e.By)
		if e.Text != "" {
			txt += " (" + e.Text + ")"
		}
		m.add(entry{kind: kNotice, text: txt})
		if len(m.approvals) > 0 && m.showDialog {
			m.openDialog()
		} else {
			m.showDialog = false
		}
	}
	m.refresh(follow)
}

func (m *Model) add(e entry) { m.entries = append(m.entries, e) }

func (m *Model) openDialog() {
	m.showDialog = true
	m.layout()
	a := m.approvals[0]
	body := a.Preview
	if body == "" {
		body = a.Args
	}
	m.dialogVP.SetContent(m.colorDiff(body))
	m.dialogLines = strings.Count(body, "\n") + 1
	m.dialogVP.Height = min(m.dialogLines, m.dialogMaxHeight())
	m.dialogVP.GotoTop()
}

func (m *Model) layout() {
	if m.width == 0 {
		return
	}
	m.input.Width = max(10, m.width-8)
	// title (1) + input box (3) + help (1) + status (1)
	m.vp.Width, m.vp.Height = m.width, max(3, m.height-6)
	m.dialogVP.Width = max(20, min(m.width-10, 120))
	m.dialogVP.Height = min(max(m.dialogLines, 1), m.dialogMaxHeight())
}

// dialogMaxHeight is the preview height that fits the body area (transcript + input box) minus the dialog's
// border, padding, title, meta, hint and spacer lines.
func (m *Model) dialogMaxHeight() int { return max(3, m.vp.Height+3-9) }

// refresh re-renders the transcript; follow keeps the view pinned to the bottom.
func (m *Model) refresh(follow bool) {
	if m.width == 0 {
		return
	}
	var b strings.Builder
	w := m.width - 2
	for i := range m.entries {
		e := &m.entries[i]
		if i > 0 && (e.kind == kUser || e.kind == kAnswer || e.kind == kIncident) {
			b.WriteString("\n")
		}
		switch e.kind {
		case kUser:
			label := "you"
			if e.ev.By != "" && e.ev.By != "tui" {
				label = e.ev.By
			}
			b.WriteString(m.st.userLabel.Render(label) + "\n")
			b.WriteString(m.st.userBody.Width(w).Render(e.text) + "\n")
		case kAnswer:
			b.WriteString(m.st.agentLabel.Render("kharness") + "\n")
			b.WriteString(m.cachedMarkdown(e, w) + "\n")
		case kTool:
			risk := ""
			switch e.ev.Risk {
			case "write":
				risk = m.st.riskWrite.Render(" [write]")
			case "dangerous":
				risk = m.st.riskDanger.Render(" [dangerous]")
			}
			args := runewidth.Truncate(strings.Join(strings.Fields(e.ev.Args), " "), max(10, w-len(e.ev.Tool)-20), "…")
			line := "  " + m.st.toolName.Render("→ "+e.ev.Tool) + risk + " " + m.st.toolArgs.Render(args)
			switch {
			case e.text == "":
				line += " " + m.spin.View()
			case strings.HasPrefix(e.text, "error"):
				line += "\n    " + m.st.toolErr.Render(runewidth.Truncate(e.text, w-4, "…"))
			}
			b.WriteString(line + "\n")
		case kNotice:
			b.WriteString(m.st.notice.Width(w).Render("• "+e.text) + "\n")
		case kError:
			b.WriteString(m.st.errorLine.Width(w).Render("✗ "+e.text) + "\n")
		case kIncident:
			title := "🚨 incident " + e.ev.ID
			if e.ev.By == "agent" {
				title = "🩺 diagnosis " + e.ev.ID
			}
			b.WriteString(m.st.incident.Render(title) + "\n")
			body := e.text
			if e.ev.By == "agent" {
				body = m.cachedMarkdown(e, w-3)
			}
			b.WriteString(m.st.incidentBody.Width(w-2).Render(body) + "\n")
		}
	}
	m.vp.SetContent(b.String())
	if follow {
		m.vp.GotoBottom()
	}
}

func (m *Model) cachedMarkdown(e *entry, width int) string {
	if e.md == "" || e.mdW != width {
		e.md, e.mdW = m.markdown(e.text, width), width
	}
	return e.md
}

func (m *Model) markdown(src string, width int) string {
	if m.renderer == nil || m.rendW != width {
		r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(GlamourStyle), glamour.WithWordWrap(width))
		if err != nil {
			return src
		}
		m.renderer, m.rendW = r, width
	}
	out, err := m.renderer.Render(src)
	if err != nil {
		return src
	}
	return strings.Trim(out, "\n")
}

func (m *Model) colorDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			lines[i] = m.st.diffHdr.Render(l)
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, "  + "):
			lines[i] = m.st.diffAdd.Render(l)
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, "  - "):
			lines[i] = m.st.diffDel.Render(l)
		case strings.HasPrefix(l, "~ "), strings.HasPrefix(l, "  ~ "):
			lines[i] = m.st.riskWrite.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	title := m.titleBar()
	footer := m.help.View(chatHelp{m.keys})
	if m.showDialog && len(m.approvals) > 0 {
		footer = m.help.View(dialogHelp{m.keys})
	}
	status := m.status
	if status == "" || status == "connected" {
		status = m.info
		if m.model != "" {
			status = "model " + m.model + " · " + status
		}
	}
	status = m.st.dim.Render(runewidth.Truncate(status, m.width, "…"))

	var body string
	if m.showDialog && len(m.approvals) > 0 {
		body = lipgloss.Place(m.width, m.vp.Height+3, lipgloss.Center, lipgloss.Center, m.dialog())
	} else {
		body = m.vp.View() + "\n" + m.st.inputBox.Width(m.width-2).Render(m.input.View())
	}
	return lipgloss.JoinVertical(lipgloss.Left, title, body, footer, status)
}

func (m Model) titleBar() string {
	left := m.st.titleBar.Render("kharness") + m.st.titleFill.Render(" "+m.session+" ")
	var badges []string
	if n := len(m.approvals); n > 0 {
		badges = append(badges, m.st.badgeAsk.Render(fmt.Sprintf("%d approval%s", n, map[bool]string{true: "", false: "s"}[n == 1])))
	}
	switch {
	case !m.connected:
		badges = append(badges, m.st.badgeErr.Render("● disconnected"))
	case m.busy:
		badges = append(badges, m.st.badgeWarn.Render(m.spin.View()+" working"))
	default:
		badges = append(badges, m.st.badgeOK.Render("● ready"))
	}
	right := strings.Join(badges, "")
	gap := max(0, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + m.st.titleFill.Render(strings.Repeat(" ", gap)) + right
}

func (m Model) dialog() string {
	a := m.approvals[0]
	risk := m.st.riskWrite.Render(a.Risk)
	if a.Risk == "dangerous" {
		risk = m.st.riskDanger.Render("DANGEROUS")
	}
	sess := a.Session
	if sess == "" {
		sess = a.By
	}
	head := m.st.dialogTitle.Render("Approve change?") + "  " + risk
	if len(m.approvals) > 1 {
		head += m.st.dim.Render(fmt.Sprintf("  (1 of %d)", len(m.approvals)))
	}
	meta := m.st.fieldLabel.Render("tool ") + m.st.toolName.Render(a.Tool) +
		m.st.fieldLabel.Render("   session ") + sess +
		m.st.fieldLabel.Render("   requested ") + a.Time.Local().Format(time.TimeOnly)
	hint := m.st.dim.Render("y approve · n deny · j/k scroll · esc decide later")
	return m.st.dialogBox.Render(lipgloss.JoinVertical(lipgloss.Left, head, meta, "", m.dialogVP.View(), "", hint))
}
