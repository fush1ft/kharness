// Package agent runs the LLM ↔ tools loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/llm"
	"github.com/fush1ft/kharness/internal/tools"
)

// Chatter is the LLM side of the loop (implemented by *llm.Client, mocked in tests).
type Chatter interface {
	Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Message, string, error)
}

// Approver decides whether a non-read tool call may run (*approval.Gate, or a terminal prompt in `chat`).
type Approver interface {
	Approve(ctx context.Context, req approval.Request) (approval.Decision, error)
}

type DenyAll struct{}

func (DenyAll) Approve(context.Context, approval.Request) (approval.Decision, error) {
	return approval.Decision{Reason: "write actions are disabled (read-only mode)", By: "policy"}, nil
}

// Observer receives progress events; every field is optional.
type Observer struct {
	ToolCall   func(name, args string, risk tools.Risk)
	ToolResult func(name, args, result string, err error)
	Model      func(model string)
}

type Agent struct {
	LLM      Chatter
	Tools    *tools.Registry
	Approver Approver
	Observer Observer
	MaxSteps int
	// Session names the conversation in approval requests.
	Session string
	// HistoryBudget is the rough size (bytes of message content) above which old turns are compacted/dropped.
	HistoryBudget int

	history []llm.Message
}

func New(c Chatter, reg *tools.Registry, system string) *Agent {
	return &Agent{
		LLM: c, Tools: reg, Approver: DenyAll{}, MaxSteps: 25, HistoryBudget: 120_000,
		history: []llm.Message{{Role: "system", Content: system}},
	}
}

var ErrMaxSteps = errors.New("agent: step limit reached")

// Run adds the user message and loops until the model answers without tool calls.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	a.history = append(a.history, llm.Message{Role: "user", Content: input})
	a.compact()
	defs := a.Tools.Defs()
	for step := 0; step < a.MaxSteps; step++ {
		reply, model, err := a.LLM.Chat(ctx, a.history, defs)
		if err != nil {
			return "", err
		}
		if a.Observer.Model != nil {
			a.Observer.Model(model)
		}
		a.history = append(a.history, reply)
		if len(reply.ToolCalls) == 0 {
			return reply.Content, nil
		}
		for _, tc := range reply.ToolCalls {
			result := a.call(ctx, tc)
			a.history = append(a.history, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", ErrMaxSteps
}

// call executes one tool call and returns what the model should see. Errors are reported back to the model
// rather than aborting the loop, so it can correct itself.
func (a *Agent) call(ctx context.Context, tc llm.ToolCall) string {
	name, rawArgs := tc.Function.Name, tc.Function.Arguments
	t, ok := a.Tools.Get(name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", name)
	}
	args := json.RawMessage(rawArgs)
	if len(rawArgs) > 0 && !json.Valid(args) {
		return "error: arguments are not valid JSON"
	}
	risk := tools.RiskOf(t, args)
	if a.Observer.ToolCall != nil {
		a.Observer.ToolCall(name, rawArgs, risk)
	}
	report := func(out string, err error) {
		if a.Observer.ToolResult != nil {
			a.Observer.ToolResult(name, rawArgs, out, err)
		}
	}
	if risk != tools.Read {
		req := approval.Request{Session: a.Session, Tool: name, Risk: risk.String(), Args: rawArgs}
		if p, ok := t.(tools.Previewer); ok {
			preview, err := p.Preview(ctx, args)
			if err != nil {
				// A failed dry-run usually means the call itself would fail; let the model fix it first.
				report("", fmt.Errorf("preview failed: %w", err))
				return "error: dry-run failed, nothing was changed: " + err.Error()
			}
			req.Preview = preview
		}
		d, err := a.Approver.Approve(ctx, req)
		if err != nil {
			report("", err)
			return "error: approval failed: " + err.Error()
		}
		if !d.OK {
			msg := "denied by operator"
			if d.Reason != "" {
				msg += ": " + d.Reason
			}
			report("", errors.New(msg))
			return msg + ". Do not retry the same action; ask the user how to proceed."
		}
	}
	out, err := t.Run(ctx, args)
	report(out, err)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// Reset drops the conversation but keeps the system prompt.
func (a *Agent) Reset() { a.history = a.history[:1] }

// compact keeps the history under HistoryBudget: first it shortens tool results of older turns, then it drops
// whole old turns. Turns are cut only at user messages so tool calls never lose their results.
func (a *Agent) compact() {
	if a.HistoryBudget <= 0 {
		return
	}
	userIdx := []int{}
	for i, m := range a.history {
		if m.Role == "user" {
			userIdx = append(userIdx, i)
		}
	}
	// Tool outputs before the previous user message are old evidence; keep only their head.
	if len(userIdx) >= 2 && a.size() > a.HistoryBudget {
		for i := 1; i < userIdx[len(userIdx)-2]; i++ {
			if m := &a.history[i]; m.Role == "tool" && len(m.Content) > 600 {
				m.Content = m.Content[:600] + "\n…(older tool output trimmed)"
			}
		}
	}
	for len(userIdx) >= 2 && a.size() > a.HistoryBudget {
		shift := userIdx[1] - 1 // the second turn moves right after the system prompt
		a.history = append(a.history[:1], a.history[userIdx[1]:]...)
		userIdx = userIdx[1:]
		for i := range userIdx {
			userIdx[i] -= shift
		}
	}
}

func (a *Agent) size() int {
	n := 0
	for _, m := range a.history {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n
}
