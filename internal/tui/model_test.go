package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/server"
)

type fakeBackend struct {
	ch   chan events.Event
	sent []server.ClientMsg
}

func (f *fakeBackend) Events() <-chan events.Event   { return f.ch }
func (f *fakeBackend) Send(m server.ClientMsg) error { f.sent = append(f.sent, m); return nil }
func (f *fakeBackend) Close() error                  { return nil }

func feed(m tea.Model, msgs ...tea.Msg) tea.Model {
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m
}

func press(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestChatAndApprovalFlow(t *testing.T) {
	be := &fakeBackend{ch: make(chan events.Event)}
	var m tea.Model = New(be, "tui")
	m = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30},
		evMsg{Type: events.Hello, Text: "cluster https://x · models m1"},
		press("why is web down?"), press("enter"))
	if len(be.sent) != 1 || be.sent[0].Type != "ask" || be.sent[0].Text != "why is web down?" {
		t.Fatalf("ask not sent: %+v", be.sent)
	}
	m = feed(m,
		evMsg{Type: events.User, Session: "tui", Text: "why is web down?", By: "tui"},
		evMsg{Type: events.Busy, Session: "tui"},
		evMsg{Type: events.ToolCall, Session: "tui", Tool: "list_events", Args: `{"namespace":"apps"}`, Risk: "read"},
		evMsg{Type: events.ToolResult, Session: "tui", Tool: "list_events"},
		evMsg{Type: events.Approval, Session: "incident", ID: "a1", Tool: "rollout_restart", Risk: "write", Preview: "+ added line\n- removed line"},
	)
	v := m.View()
	for _, want := range []string{"Approve change?", "rollout_restart", "added line", "1 approval", "working"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	// "n" in the dialog denies; it must not be typed into the input.
	m = feed(m, press("n"))
	if last := be.sent[len(be.sent)-1]; last.Type != "approve" || last.ID != "a1" || last.OK {
		t.Fatalf("deny not sent: %+v", last)
	}
	m = feed(m,
		evMsg{Type: events.ApprovalResolved, ID: "a1", Tool: "rollout_restart", OK: false, By: "tui"},
		evMsg{Type: events.Answer, Session: "tui", Text: "The **web** pod crashes."},
		evMsg{Type: events.Idle, Session: "tui"},
	)
	v = m.View()
	for _, want := range []string{"list_events", "rollout_restart denied by tui", "web", "ready"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Approve change?") {
		t.Error("dialog still open after resolution")
	}
	m = feed(m, press("/reset"), press("enter"))
	if last := be.sent[len(be.sent)-1]; last.Type != "reset" {
		t.Fatalf("reset not sent: %+v", last)
	}
}

func TestEscCancelsRunningTurn(t *testing.T) {
	be := &fakeBackend{ch: make(chan events.Event)}
	var m tea.Model = New(be, "tui")
	m = feed(m, tea.WindowSizeMsg{Width: 80, Height: 24}, evMsg{Type: events.Busy, Session: "tui"}, press("esc"))
	if len(be.sent) != 1 || be.sent[0].Type != "cancel" {
		t.Fatalf("cancel not sent: %+v", be.sent)
	}
}
