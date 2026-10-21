package server

import (
	"context"
	"errors"

	"github.com/fush1ft/kharness/internal/events"
)

// Backend is what the TUI talks to: the remote WebSocket client or the in-process Local backend.
type Backend interface {
	Events() <-chan events.Event
	Send(ClientMsg) error
	Close() error
}

// Local attaches to a Core in the same process (`kharness tui --local`).
type Local struct {
	events chan events.Event
	in     chan ClientMsg
	cancel context.CancelFunc
}

func NewLocal(c *Core, sess, by string) *Local {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Local{events: make(chan events.Event, 256), in: make(chan ClientMsg, 16), cancel: cancel}
	go func() {
		_ = c.Attach(ctx, sess, by,
			func(e events.Event) error {
				select {
				case l.events <- e:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			func() (ClientMsg, error) {
				select {
				case m := <-l.in:
					return m, nil
				case <-ctx.Done():
					return ClientMsg{}, ctx.Err()
				}
			})
	}()
	return l
}

func (l *Local) Events() <-chan events.Event { return l.events }

func (l *Local) Send(m ClientMsg) error {
	select {
	case l.in <- m:
		return nil
	default:
		return errors.New("backend busy")
	}
}

func (l *Local) Close() error { l.cancel(); return nil }
