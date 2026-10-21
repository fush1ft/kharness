// Package tools defines the tool interface the agent calls and a registry of available tools.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/fush1ft/kharness/internal/llm"
)

// Risk says whether a tool may run freely or must pass the approval gate.
type Risk int

const (
	Read      Risk = iota // no side effects, runs without approval
	Write                 // changes cluster state, needs approval
	Dangerous             // destructive (delete, arbitrary shell), needs approval and is flagged loudly
)

func (r Risk) String() string {
	return [...]string{"read", "write", "dangerous"}[r]
}

type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema of the arguments object.
	Schema() json.RawMessage
	Risk() Risk
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// RiskAssessor is implemented by tools whose risk depends on the arguments (e.g. the kubectl fallback).
type RiskAssessor interface {
	RiskFor(args json.RawMessage) Risk
}

// Previewer is implemented by write tools that can show what they would change (server-side dry-run diff,
// rendered chart summary) before the operator approves.
type Previewer interface {
	Preview(ctx context.Context, args json.RawMessage) (string, error)
}

// RiskOf returns the effective risk of a call.
func RiskOf(t Tool, args json.RawMessage) Risk {
	if ra, ok := t.(RiskAssessor); ok {
		return ra.RiskFor(args)
	}
	return t.Risk()
}

type Registry struct {
	tools map[string]Tool
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t Tool) {
	if _, dup := r.tools[t.Name()]; dup {
		panic(fmt.Sprintf("tools: duplicate tool %q", t.Name()))
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Defs returns the tool definitions in a stable order for the LLM request.
func (r *Registry) Defs() []llm.ToolDef {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, llm.ToolDef{Type: "function", Function: llm.FunctionDef{
			Name: t.Name(), Description: t.Description(), Parameters: t.Schema(),
		}})
	}
	return defs
}

// Func adapts plain functions into a Tool. P (preview) is optional.
type Func struct {
	N, D string
	S    json.RawMessage
	R    Risk
	F    func(ctx context.Context, args json.RawMessage) (string, error)
	P    func(ctx context.Context, args json.RawMessage) (string, error)
}

func (f *Func) Preview(ctx context.Context, args json.RawMessage) (string, error) {
	if f.P == nil {
		return "", nil
	}
	return f.P(ctx, args)
}

func (f *Func) Name() string            { return f.N }
func (f *Func) Description() string     { return f.D }
func (f *Func) Schema() json.RawMessage { return f.S }
func (f *Func) Risk() Risk              { return f.R }
func (f *Func) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return f.F(ctx, args)
}
