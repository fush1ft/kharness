// Package approval is the gate every non-read tool call passes through. Requests are published on the event bus;
// any front-end (TUI, Telegram, CLI prompt) may resolve them, first answer wins.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/fush1ft/kharness/internal/events"
)

type Request struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	Tool    string    `json:"tool"`
	Risk    string    `json:"risk"`
	Args    string    `json:"args"`
	Preview string    `json:"preview"`
	Created time.Time `json:"created"`
}

type Decision struct {
	OK     bool
	Reason string
	By     string
}

type Gate struct {
	Bus     *events.Bus
	Timeout time.Duration
	// OnDecision, if set, is called for every resolved request (audit log).
	OnDecision func(Request, Decision)

	mu      sync.Mutex
	pending map[string]*pending
}

type pending struct {
	req  Request
	done chan Decision
}

var ErrUnknown = errors.New("approval: unknown or already resolved request")

func NewGate(bus *events.Bus) *Gate {
	return &Gate{Bus: bus, Timeout: 15 * time.Minute, pending: map[string]*pending{}}
}

// Approve publishes the request and blocks until someone resolves it, the timeout passes (deny) or ctx ends.
func (g *Gate) Approve(ctx context.Context, req Request) (Decision, error) {
	req.ID = newID()
	req.Created = time.Now()
	p := &pending{req: req, done: make(chan Decision, 1)}
	g.mu.Lock()
	g.pending[req.ID] = p
	g.mu.Unlock()

	g.Bus.Publish(events.Event{
		Type: events.Approval, Session: req.Session, ID: req.ID,
		Tool: req.Tool, Risk: req.Risk, Args: req.Args, Preview: req.Preview,
	})

	timer := time.NewTimer(g.Timeout)
	defer timer.Stop()
	select {
	case d := <-p.done:
		return d, nil
	case <-timer.C:
		// If someone resolved it concurrently, Resolve is a no-op and their decision is already in p.done.
		_ = g.Resolve(req.ID, Decision{OK: false, Reason: "timed out waiting for approval", By: "timeout"})
		return <-p.done, nil
	case <-ctx.Done():
		_ = g.Resolve(req.ID, Decision{OK: false, Reason: "cancelled", By: "system"})
		return Decision{}, ctx.Err()
	}
}

// Resolve answers a pending request.
func (g *Gate) Resolve(id string, d Decision) error {
	g.mu.Lock()
	p, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	p.done <- d
	if g.OnDecision != nil {
		g.OnDecision(p.req, d)
	}
	g.Bus.Publish(events.Event{
		Type: events.ApprovalResolved, Session: p.req.Session, ID: id, Tool: p.req.Tool, OK: d.OK, By: d.By, Text: d.Reason,
	})
	return nil
}

// Pending lists unresolved requests, oldest first (sent to clients that connect late).
func (g *Gate) Pending() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Request, 0, len(g.pending))
	for _, p := range g.pending {
		out = append(out, p.req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
