package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
)

// fakeBot records Bot API calls and serves one callback update that approves the first approval message.
type fakeBot struct {
	mu        sync.Mutex
	calls     map[string][]map[string]any
	callback  string
	delivered bool
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.calls[method] = append(f.calls[method], req)
	cb := f.callback
	f.mu.Unlock()
	switch method {
	case "getMe":
		_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"testbot"}}`))
	case "getUpdates":
		f.mu.Lock()
		send := cb != "" && !f.delivered
		if send {
			f.delivered = true
		}
		f.mu.Unlock()
		if send {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"callback_query":{"id":"q1","data":"` + cb +
				`","from":{"id":5,"username":"op"},"message":{"message_id":10,"chat":{"id":42}}}}]}`))
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	case "sendMessage":
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":10}}`))
	default:
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

func (f *fakeBot) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[method])
}

func TestTelegramApprovalFlow(t *testing.T) {
	bot := &fakeBot{calls: map[string][]map[string]any{}}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	bus := events.NewBus()
	gate := approval.NewGate(bus)
	log, _ := audit.Open("")
	tg := &Telegram{Token: "T", Chats: []int64{42}, Gate: gate, Bus: bus, Audit: log, BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.Run(ctx)
	time.Sleep(100 * time.Millisecond) // let forward() subscribe

	done := make(chan approval.Decision, 1)
	go func() {
		d, _ := gate.Approve(ctx, approval.Request{Tool: "scale", Risk: "write", Session: "incident", Preview: "replicas <1> → 3"})
		done <- d
	}()
	// Wait for the approval message, then let the fake deliver the button press.
	deadline := time.Now().Add(2 * time.Second)
	for bot.count("sendMessage") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	id := gate.Pending()[0].ID
	bot.mu.Lock()
	msg := bot.calls["sendMessage"][0]
	bot.callback = "ap:" + id + ":y"
	bot.mu.Unlock()
	if txt := msg["text"].(string); !strings.Contains(txt, "replicas &lt;1&gt; → 3") || msg["reply_markup"] == nil {
		t.Fatalf("approval message not escaped or without buttons: %v", msg)
	}

	select {
	case d := <-done:
		if !d.OK || d.By != "telegram:@op" {
			t.Fatalf("decision %+v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval not resolved via callback")
	}
	deadline = time.Now().Add(2 * time.Second)
	for bot.count("editMessageText") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bot.count("editMessageText") != 1 || bot.count("answerCallbackQuery") != 1 {
		t.Fatalf("edit=%d answer=%d", bot.count("editMessageText"), bot.count("answerCallbackQuery"))
	}
}

func TestSplit(t *testing.T) {
	parts := split(strings.Repeat("line\n", 2000), 3500)
	for _, p := range parts {
		if len(p) > 3500 {
			t.Fatalf("chunk of %d", len(p))
		}
	}
	if strings.Join(parts, "\n") != strings.TrimSpace(strings.Repeat("line\n", 2000)) {
		t.Fatal("split lost text")
	}
}
