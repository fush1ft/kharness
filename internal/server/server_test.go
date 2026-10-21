package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fush1ft/kharness/internal/agent"
	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/client"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/llm"
	"github.com/fush1ft/kharness/internal/server"
	"github.com/fush1ft/kharness/internal/session"
	"github.com/fush1ft/kharness/internal/tools"
)

// scripted asks for a write tool on the first turn and answers once the result came back.
type scripted struct{}

func (scripted) Chat(_ context.Context, msgs []llm.Message, _ []llm.ToolDef) (llm.Message, string, error) {
	last := msgs[len(msgs)-1]
	if last.Role == "tool" {
		return llm.Message{Role: "assistant", Content: "result: " + last.Content}, "mock", nil
	}
	tc := llm.ToolCall{ID: "c1", Type: "function"}
	tc.Function.Name, tc.Function.Arguments = "scale", `{"replicas":3}`
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}, "mock", nil
}

func setup(t *testing.T) (*server.Core, *atomic.Int32) {
	var ran atomic.Int32
	reg := tools.NewRegistry(&tools.Func{N: "scale", R: tools.Write, S: json.RawMessage(`{}`),
		F: func(context.Context, json.RawMessage) (string, error) { ran.Add(1); return "scaled to 3", nil },
		P: func(context.Context, json.RawMessage) (string, error) { return "replicas 1 → 3", nil }})
	bus := events.NewBus()
	gate := approval.NewGate(bus)
	log, err := audit.Open(t.TempDir() + "/audit.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	gate.OnDecision = func(r approval.Request, d approval.Decision) {
		log.Add(audit.Entry{Session: r.Session, Kind: "approval", Tool: r.Tool, OK: d.OK, Actor: d.By})
	}
	sessions := session.NewManager(bus, log, func(string) *agent.Agent {
		a := agent.New(scripted{}, reg, "sys")
		a.Approver = gate
		return a
	})
	return &server.Core{Sessions: sessions, Gate: gate, Bus: bus, Audit: log, Info: "test"}, &ran
}

func next(t *testing.T, ch <-chan events.Event, typ string) events.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed waiting for %s", typ)
			}
			if e.Type == typ {
				return e
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func TestWebSocketAskApproveAnswer(t *testing.T) {
	core, ran := setup(t)
	srv := httptest.NewServer(core.Handler("/kharness", "secret"))
	defer srv.Close()

	if _, err := client.Dial(client.Config{URL: srv.URL + "/kharness", Token: "wrong"}, "tui", "me"); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token accepted: %v", err)
	}
	r, err := client.Dial(client.Config{URL: srv.URL + "/kharness", Token: "secret"}, "tui", "me")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if e := next(t, r.Events(), events.Hello); e.Text != "test" {
		t.Fatalf("hello: %+v", e)
	}
	if err := r.Send(server.ClientMsg{Type: "ask", Text: "scale web"}); err != nil {
		t.Fatal(err)
	}
	ap := next(t, r.Events(), events.Approval)
	if ap.Preview != "replicas 1 → 3" || ap.Risk != "write" {
		t.Fatalf("approval: %+v", ap)
	}
	if ran.Load() != 0 {
		t.Fatal("tool ran before approval")
	}
	if err := r.Send(server.ClientMsg{Type: "approve", ID: ap.ID, OK: true}); err != nil {
		t.Fatal(err)
	}
	if e := next(t, r.Events(), events.ApprovalResolved); !e.OK || e.By != "me" {
		t.Fatalf("resolved: %+v", e)
	}
	if e := next(t, r.Events(), events.Answer); e.Text != "result: scaled to 3" {
		t.Fatalf("answer: %+v", e)
	}
	if ran.Load() != 1 {
		t.Fatal("tool did not run once")
	}

	// The audit log saw the question, the approval and the tool call.
	if err := r.Send(server.ClientMsg{Type: "audit"}); err != nil {
		t.Fatal(err)
	}
	n := next(t, r.Events(), events.Notice)
	for _, want := range []string{"question", "approval", "scale"} {
		if !strings.Contains(n.Text, want) {
			t.Errorf("audit missing %q:\n%s", want, n.Text)
		}
	}
}

func TestLocalBackendDenyAndPendingReplay(t *testing.T) {
	core, ran := setup(t)
	// Start a turn from another session before the client attaches: its approval must be replayed.
	go core.Sessions.Ask(context.Background(), "other", "x", "scale", false)
	time.Sleep(100 * time.Millisecond)
	l := server.NewLocal(core, "tui", "local")
	defer l.Close()
	ap := next(t, l.Events(), events.Approval)
	if ap.By != "other" {
		t.Fatalf("replayed approval should carry its session: %+v", ap)
	}
	_ = l.Send(server.ClientMsg{Type: "approve", ID: ap.ID, OK: false})
	next(t, l.Events(), events.ApprovalResolved)
	time.Sleep(100 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("denied tool ran")
	}
}

func TestHealthz(t *testing.T) {
	core, _ := setup(t)
	srv := httptest.NewServer(core.Handler("/kharness", "secret"))
	defer srv.Close()
	for _, p := range []string{"/healthz", "/kharness/healthz"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, resp, err)
		}
	}
}
