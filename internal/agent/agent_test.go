package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fush1ft/kharness/internal/llm"
	"github.com/fush1ft/kharness/internal/tools"
)

// scripted replays canned replies and records what the model was sent.
type scripted struct {
	replies []llm.Message
	seen    [][]llm.Message
}

func (s *scripted) Chat(_ context.Context, msgs []llm.Message, _ []llm.ToolDef) (llm.Message, string, error) {
	s.seen = append(s.seen, append([]llm.Message(nil), msgs...))
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, "mock", nil
}

func toolCall(id, name, args string) llm.Message {
	tc := llm.ToolCall{ID: id, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}
}

func TestRunExecutesReadToolAndReturnsAnswer(t *testing.T) {
	ran := false
	reg := tools.NewRegistry(&tools.Func{N: "ping", R: tools.Read, S: json.RawMessage(`{}`),
		F: func(context.Context, json.RawMessage) (string, error) { ran = true; return "pong", nil }})
	m := &scripted{replies: []llm.Message{toolCall("1", "ping", "{}"), {Role: "assistant", Content: "done"}}}

	out, err := New(m, reg, "sys").Run(context.Background(), "hi")
	if err != nil || out != "done" || !ran {
		t.Fatalf("out=%q err=%v ran=%v", out, err, ran)
	}
	last := m.seen[1][len(m.seen[1])-1]
	if last.Role != "tool" || last.ToolCallID != "1" || last.Content != "pong" {
		t.Fatalf("tool result not fed back: %+v", last)
	}
}

func TestWriteToolIsDeniedByDefault(t *testing.T) {
	ran := false
	reg := tools.NewRegistry(&tools.Func{N: "delete_pod", R: tools.Dangerous, S: json.RawMessage(`{}`),
		F: func(context.Context, json.RawMessage) (string, error) { ran = true; return "", nil }})
	m := &scripted{replies: []llm.Message{toolCall("1", "delete_pod", "{}"), {Role: "assistant", Content: "ok"}}}

	if _, err := New(m, reg, "sys").Run(context.Background(), "delete it"); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("dangerous tool ran without approval")
	}
	if got := m.seen[1][len(m.seen[1])-1].Content; !strings.HasPrefix(got, "denied") {
		t.Fatalf("model not told about denial: %q", got)
	}
}

func TestUnknownToolAndBadJSONAreReportedToModel(t *testing.T) {
	reg := tools.NewRegistry(&tools.Func{N: "ping", R: tools.Read, S: json.RawMessage(`{}`),
		F: func(context.Context, json.RawMessage) (string, error) { return "pong", nil }})
	m := &scripted{replies: []llm.Message{
		toolCall("1", "nope", "{}"), toolCall("2", "ping", "{bad"), {Role: "assistant", Content: "ok"},
	}}
	if _, err := New(m, reg, "sys").Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if got := m.seen[1][len(m.seen[1])-1].Content; !strings.Contains(got, "unknown tool") {
		t.Fatalf("got %q", got)
	}
	if got := m.seen[2][len(m.seen[2])-1].Content; !strings.Contains(got, "not valid JSON") {
		t.Fatalf("got %q", got)
	}
}

func TestStepLimit(t *testing.T) {
	reg := tools.NewRegistry(&tools.Func{N: "ping", R: tools.Read, S: json.RawMessage(`{}`),
		F: func(context.Context, json.RawMessage) (string, error) { return "pong", nil }})
	m := &scripted{}
	for range 3 {
		m.replies = append(m.replies, toolCall("1", "ping", "{}"))
	}
	a := New(m, reg, "sys")
	a.MaxSteps = 3
	if _, err := a.Run(context.Background(), "loop"); err != ErrMaxSteps {
		t.Fatalf("err = %v", err)
	}
}

func TestCompactKeepsTurnsWhole(t *testing.T) {
	a := New(&scripted{}, tools.NewRegistry(), "sys")
	a.HistoryBudget = 3000
	for i := range 6 {
		tc := toolCall("c", "ping", "{}")
		a.history = append(a.history,
			llm.Message{Role: "user", Content: strings.Repeat("q", 100)},
			tc,
			llm.Message{Role: "tool", ToolCallID: "c", Content: strings.Repeat("x", 1000)},
			llm.Message{Role: "assistant", Content: "a" + string(rune('0'+i))},
		)
	}
	a.history = append(a.history, llm.Message{Role: "user", Content: "latest"})
	a.compact()
	if a.size() > a.HistoryBudget {
		t.Fatalf("size %d over budget", a.size())
	}
	if a.history[0].Role != "system" || a.history[1].Role != "user" || a.history[len(a.history)-1].Content != "latest" {
		t.Fatalf("bad shape: first=%s second=%s", a.history[0].Role, a.history[1].Role)
	}
	for i, m := range a.history {
		if m.Role == "tool" && (i == 0 || len(a.history[i-1].ToolCalls) == 0) {
			t.Fatalf("tool message %d lost its call", i)
		}
	}
}
