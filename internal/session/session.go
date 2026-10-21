// Package session keeps one agent conversation per session key ("tui", "tg:<chat>", "incident") and publishes
// everything that happens in it on the event bus.
package session

import (
	"context"
	"errors"
	"sync"

	"github.com/fush1ft/kharness/internal/agent"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/tools"
)

var ErrBusy = errors.New("the agent is still working on the previous question (cancel it or wait)")

// Factory builds a fresh agent; the manager sets Session and Observer on it.
type Factory func(session string) *agent.Agent

type Manager struct {
	Bus   *events.Bus
	Audit *audit.Log
	New   Factory

	mu       sync.Mutex
	sessions map[string]*entry
}

type entry struct {
	a      *agent.Agent
	sem    chan struct{} // one turn at a time
	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewManager(bus *events.Bus, log *audit.Log, f Factory) *Manager {
	return &Manager{Bus: bus, Audit: log, New: f, sessions: map[string]*entry{}}
}

func (m *Manager) get(session string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.sessions[session]; ok {
		return e
	}
	a := m.New(session)
	a.Session = session
	a.Observer = agent.Observer{
		Model: func(model string) { m.Bus.Publish(events.Event{Type: events.Model, Session: session, Text: model}) },
		ToolCall: func(name, args string, risk tools.Risk) {
			m.Bus.Publish(events.Event{Type: events.ToolCall, Session: session, Tool: name, Args: args, Risk: risk.String()})
		},
		ToolResult: func(name, args, result string, err error) {
			ev := events.Event{Type: events.ToolResult, Session: session, Tool: name, Text: clip(result, 2000)}
			entry := audit.Entry{Session: session, Kind: "tool", Tool: name, Args: args, OK: err == nil, Detail: clip(result, 500)}
			if t, ok := a.Tools.Get(name); ok {
				entry.Risk = tools.RiskOf(t, []byte(args)).String()
			}
			if err != nil {
				ev.Error = err.Error()
				entry.Detail = err.Error()
			}
			m.Bus.Publish(ev)
			m.Audit.Add(entry)
		},
	}
	e := &entry{a: a, sem: make(chan struct{}, 1)}
	m.sessions[session] = e
	return e
}

// Ask runs one turn. With wait=false it fails fast with ErrBusy if the session is working; with wait=true it
// queues behind the running turn.
func (m *Manager) Ask(ctx context.Context, session, by, text string, wait bool) (string, error) {
	e := m.get(session)
	if wait {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		select {
		case e.sem <- struct{}{}:
		default:
			return "", ErrBusy
		}
	}
	defer func() { <-e.sem }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()

	m.Bus.Publish(events.Event{Type: events.User, Session: session, Text: text, By: by})
	m.Bus.Publish(events.Event{Type: events.Busy, Session: session})
	m.Audit.Add(audit.Entry{Session: session, Kind: "question", Actor: by, OK: true, Detail: text})
	answer, err := e.a.Run(ctx, text)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			err = errors.New("cancelled")
		}
		m.Bus.Publish(events.Event{Type: events.Error, Session: session, Error: err.Error()})
	} else {
		m.Bus.Publish(events.Event{Type: events.Answer, Session: session, Text: answer})
	}
	m.Bus.Publish(events.Event{Type: events.Idle, Session: session})
	return answer, err
}

// Cancel stops the running turn of a session; it reports whether one was running.
func (m *Manager) Cancel(session string) bool {
	e := m.get(session)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel == nil || len(e.sem) == 0 {
		return false
	}
	e.cancel()
	return true
}

func (m *Manager) Busy(session string) bool { return len(m.get(session).sem) > 0 }

// Reset clears the conversation of an idle session.
func (m *Manager) Reset(session string) error {
	e := m.get(session)
	select {
	case e.sem <- struct{}{}:
		e.a.Reset()
		<-e.sem
		return nil
	default:
		return ErrBusy
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
