// Package events is the in-process pub/sub that connects agent sessions, the approval gate, the watcher and the
// front-ends (WebSocket clients, Telegram). Event is also the server→client wire format.
package events

import (
	"sync"
	"time"
)

// Event types.
const (
	User             = "user"              // a question entered a session (Text, By = source)
	Busy             = "busy"              // session started working
	Idle             = "idle"              // session finished
	Model            = "model"             // model that produced the last reply (Text)
	ToolCall         = "tool_call"         // Tool, Args, Risk
	ToolResult       = "tool_result"       // Tool, Text (truncated), Error
	Answer           = "answer"            // final reply of a turn (Text)
	Error            = "error"             // turn failed (Error)
	Approval         = "approval"          // approval requested (ID, Tool, Risk, Args, Preview)
	ApprovalResolved = "approval_resolved" // ID, OK, By, Text = reason
	Incident         = "incident"          // watcher/alert detected something (ID, Text)
	Notice           = "notice"            // informational line (Text)
	Hello            = "hello"             // first message to a client (Text = server info)
)

type Event struct {
	Type    string    `json:"type"`
	Session string    `json:"session,omitempty"`
	Text    string    `json:"text,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Args    string    `json:"args,omitempty"`
	Risk    string    `json:"risk,omitempty"`
	ID      string    `json:"id,omitempty"`
	Preview string    `json:"preview,omitempty"`
	OK      bool      `json:"ok,omitempty"`
	By      string    `json:"by,omitempty"`
	Error   string    `json:"error,omitempty"`
	Time    time.Time `json:"time"`
}

// Global reports whether every client should see the event regardless of which session it is attached to.
func (e Event) Global() bool {
	switch e.Type {
	case Approval, ApprovalResolved, Incident, Notice:
		return true
	}
	return false
}

type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func NewBus() *Bus { return &Bus{subs: map[int]chan Event{}} }

// Subscribe returns a buffered channel of events and a function that unsubscribes and closes it.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, buf)
	b.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish never blocks: a subscriber whose buffer is full misses the event.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
