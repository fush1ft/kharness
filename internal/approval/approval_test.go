package approval

import (
	"context"
	"testing"
	"time"

	"github.com/fush1ft/kharness/internal/events"
)

func TestApproveResolvedByOtherParty(t *testing.T) {
	bus := events.NewBus()
	ch, unsub := bus.Subscribe(8)
	defer unsub()
	g := NewGate(bus)
	go func() {
		e := <-ch // the approval request
		if e.Type != events.Approval || e.Tool != "scale" {
			t.Errorf("unexpected event %+v", e)
		}
		if len(g.Pending()) != 1 {
			t.Errorf("pending = %d", len(g.Pending()))
		}
		_ = g.Resolve(e.ID, Decision{OK: true, By: "tester"})
	}()
	d, err := g.Approve(context.Background(), Request{Tool: "scale", Session: "s"})
	if err != nil || !d.OK || d.By != "tester" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if e := <-ch; e.Type != events.ApprovalResolved || !e.OK {
		t.Fatalf("resolution not published: %+v", e)
	}
	if len(g.Pending()) != 0 {
		t.Fatal("request still pending")
	}
}

func TestApproveTimesOutAsDenied(t *testing.T) {
	g := NewGate(events.NewBus())
	g.Timeout = 20 * time.Millisecond
	var logged Decision
	g.OnDecision = func(_ Request, d Decision) { logged = d }
	d, err := g.Approve(context.Background(), Request{Tool: "x"})
	if err != nil || d.OK || d.By != "timeout" || logged.By != "timeout" {
		t.Fatalf("d=%+v logged=%+v err=%v", d, logged, err)
	}
	if err := g.Resolve("nope", Decision{}); err != ErrUnknown {
		t.Fatalf("err = %v", err)
	}
}
