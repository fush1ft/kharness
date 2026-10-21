// Package server connects front-ends to sessions: the WebSocket API used by the TUI, an in-process backend for
// `kharness tui --local`, and the HTTP server that hosts both plus the Alertmanager webhook.
package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/session"
)

// ClientMsg is the client→server wire format.
type ClientMsg struct {
	Type    string `json:"type"` // ask | cancel | reset | approve | audit
	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"`
	OK      bool   `json:"ok,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Changes bool   `json:"changes,omitempty"` // audit: only changes
}

type Core struct {
	Sessions *session.Manager
	Gate     *approval.Gate
	Bus      *events.Bus
	Audit    *audit.Log
	// Info is sent to clients in the hello event (cluster, models, mode).
	Info string
}

// Attach serves one client until recv fails or ctx ends. Events of the client's session and global events are
// passed to send; pending approvals are replayed first so a late client can still answer them.
func (c *Core) Attach(ctx context.Context, sess, by string, send func(events.Event) error, recv func() (ClientMsg, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, unsub := c.Bus.Subscribe(256)
	defer unsub()

	out := make(chan events.Event, 64)
	errc := make(chan error, 2)
	go func() {
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					return
				}
				if e.Session == sess || e.Global() {
					if err := send(e); err != nil {
						errc <- err
						return
					}
				}
			case e := <-out:
				if err := send(e); err != nil {
					errc <- err
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	reply := func(e events.Event) {
		e.Session = sess
		select {
		case out <- e:
		case <-ctx.Done():
		}
	}

	reply(events.Event{Type: events.Hello, Text: c.Info, OK: c.Sessions.Busy(sess)})
	for _, r := range c.Gate.Pending() {
		reply(events.Event{Type: events.Approval, ID: r.ID, Tool: r.Tool, Risk: r.Risk, Args: r.Args, Preview: r.Preview, By: r.Session})
	}

	go func() {
		for {
			m, err := recv()
			if err != nil {
				errc <- err
				return
			}
			c.handle(ctx, sess, by, m, reply)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Core) handle(ctx context.Context, sess, by string, m ClientMsg, reply func(events.Event)) {
	switch m.Type {
	case "ask":
		if m.Text == "" {
			return
		}
		go func() {
			// Turns outlive the connection that started them; results are on the bus and in the audit log.
			if _, err := c.Sessions.Ask(context.WithoutCancel(ctx), sess, by, m.Text, false); errors.Is(err, session.ErrBusy) {
				reply(events.Event{Type: events.Error, Error: err.Error()})
			}
		}()
	case "cancel":
		if !c.Sessions.Cancel(sess) {
			reply(events.Event{Type: events.Notice, Text: "nothing to cancel"})
		}
	case "reset":
		if err := c.Sessions.Reset(sess); err != nil {
			reply(events.Event{Type: events.Error, Error: err.Error()})
			return
		}
		reply(events.Event{Type: events.Notice, Text: "conversation cleared"})
	case "approve":
		d := approval.Decision{OK: m.OK, Reason: m.Reason, By: by}
		if err := c.Gate.Resolve(m.ID, d); err != nil {
			reply(events.Event{Type: events.Error, Error: err.Error()})
		}
	case "audit":
		es, err := c.Audit.Recent(40, m.Changes)
		if err != nil {
			reply(events.Event{Type: events.Error, Error: err.Error()})
			return
		}
		reply(events.Event{Type: events.Notice, Text: "audit log:\n" + audit.Format(es)})
	default:
		reply(events.Event{Type: events.Error, Error: fmt.Sprintf("unknown message type %q", m.Type)})
	}
}
