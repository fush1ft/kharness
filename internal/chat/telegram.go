// Package chat is the Telegram front-end: allowlisted chats talk to the agent, receive incidents and approve or
// deny changes with inline buttons. It uses the Bot API directly (long polling, no webhook, no dependency).
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/session"
)

const (
	maxMessage = 4000 // Telegram limit is 4096 characters
	// chunk is the raw-text size per message; HTML escaping can grow it, so it stays well under maxMessage.
	chunk = 3500
)

type Telegram struct {
	Token    string
	Chats    []int64 // allowlist; also the recipients of incidents and approvals
	Sessions *session.Manager
	Gate     *approval.Gate
	Bus      *events.Bus
	Audit    *audit.Log
	BaseURL  string // default https://api.telegram.org
	HTTP     *http.Client

	mu       sync.Mutex
	approvMs map[string][]sent // approval id → messages to update when resolved
}

type sent struct {
	chat int64
	msg  int
	text string
}

func (t *Telegram) api(ctx context.Context, method string, req, resp any) error {
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+t.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	c := t.HTTP
	if c == nil {
		c = &http.Client{Timeout: 70 * time.Second}
	}
	r, err := c.Do(hr)
	if err != nil {
		// Never let the token leak via the URL in *url.Error.
		return fmt.Errorf("telegram %s: %s", method, strings.ReplaceAll(err.Error(), t.Token, "<token>"))
	}
	defer r.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if resp != nil {
		return json.Unmarshal(env.Result, resp)
	}
	return nil
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From user `json:"from"`
	} `json:"message"`
	CallbackQuery *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		From    user   `json:"from"`
		Message *struct {
			MessageID int `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

func (u user) name() string {
	if u.Username != "" {
		return "telegram:@" + u.Username
	}
	return "telegram:" + u.First
}

// Run polls for updates and forwards bus events until ctx ends.
func (t *Telegram) Run(ctx context.Context) error {
	if t.Token == "" {
		return errors.New("telegram: no token")
	}
	t.approvMs = map[string][]sent{}
	var me struct {
		Username string `json:"username"`
	}
	if err := t.api(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	slog.Info("telegram bot started", "bot", me.Username, "chats", t.Chats)
	go t.forward(ctx)

	var offset int64
	for ctx.Err() == nil {
		var ups []update
		err := t.api(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("telegram poll", "err", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			t.handle(ctx, u)
		}
	}
	return nil
}

func (t *Telegram) allowed(chat int64) bool { return slices.Contains(t.Chats, chat) }

func (t *Telegram) handle(ctx context.Context, u update) {
	if cq := u.CallbackQuery; cq != nil {
		if cq.Message == nil || !t.allowed(cq.Message.Chat.ID) {
			return
		}
		t.callback(ctx, cq.ID, cq.Data, cq.From)
		return
	}
	m := u.Message
	if m == nil || m.Text == "" {
		return
	}
	if !t.allowed(m.Chat.ID) {
		slog.Warn("telegram: message from chat not in allowlist", "chat", m.Chat.ID)
		t.send(ctx, m.Chat.ID, fmt.Sprintf("This chat (%d) is not allowed. Add it to TELEGRAM_CHAT_IDS.", m.Chat.ID), nil)
		return
	}
	sess := "tg:" + strconv.FormatInt(m.Chat.ID, 10)
	text := strings.TrimSpace(m.Text)
	cmd, _, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /reset@botname in groups
	switch cmd {
	case "/start", "/help":
		t.send(ctx, m.Chat.ID, "kharness — write a question about the cluster.\n/reset clears the conversation, "+
			"/cancel stops the running question, /audit shows recent changes.", nil)
		return
	case "/reset":
		msg := "Conversation cleared."
		if err := t.Sessions.Reset(sess); err != nil {
			msg = err.Error()
		}
		t.send(ctx, m.Chat.ID, msg, nil)
		return
	case "/cancel":
		if !t.Sessions.Cancel(sess) {
			t.send(ctx, m.Chat.ID, "Nothing is running.", nil)
		}
		return
	case "/audit":
		es, err := t.Audit.Recent(20, true)
		msg := audit.Format(es)
		if err != nil {
			msg = err.Error()
		}
		t.send(ctx, m.Chat.ID, "<pre>"+html.EscapeString(clip(msg, chunk))+"</pre>", nil)
		return
	}
	go func() {
		stop := t.typing(ctx, m.Chat.ID)
		ans, err := t.Sessions.Ask(context.WithoutCancel(ctx), sess, m.From.name(), text, false)
		stop()
		if err != nil {
			t.send(ctx, m.Chat.ID, "⚠ "+html.EscapeString(err.Error()), nil)
			return
		}
		for _, part := range split(ans, chunk) {
			t.send(ctx, m.Chat.ID, html.EscapeString(part), nil)
		}
	}()
}

// typing shows "typing…" until stop is called (Telegram clears it after ~5s).
func (t *Telegram) typing(ctx context.Context, chat int64) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			_ = t.api(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
			select {
			case <-time.After(4 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel
}

func (t *Telegram) callback(ctx context.Context, id, data string, from user) {
	ack := func(text string) {
		_ = t.api(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ap" {
		ack("unknown action")
		return
	}
	ok := parts[2] == "y"
	d := approval.Decision{OK: ok, By: from.name()}
	if !ok {
		d.Reason = "denied in Telegram"
	}
	if err := t.Gate.Resolve(parts[1], d); err != nil {
		ack("already resolved")
		return
	}
	ack(map[bool]string{true: "approved", false: "denied"}[ok])
}

// forward pushes approvals, their resolution and incidents to every allowed chat.
func (t *Telegram) forward(ctx context.Context) {
	ch, unsub := t.Bus.Subscribe(128)
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			switch e.Type {
			case events.Approval:
				text := fmt.Sprintf("🔐 <b>Approval needed</b> (%s)\nsession: %s\ntool: <code>%s</code>\n",
					html.EscapeString(e.Risk), html.EscapeString(e.Session), html.EscapeString(e.Tool))
				body := e.Preview
				if body == "" {
					body = e.Args
				}
				// Leave room for escaping and for the verdict line appended when the approval is resolved.
				text += "<pre>" + html.EscapeString(clip(body, chunk-len(text)-300)) + "</pre>"
				kb := map[string]any{"inline_keyboard": [][]map[string]string{{
					{"text": "✅ Approve", "callback_data": "ap:" + e.ID + ":y"},
					{"text": "❌ Deny", "callback_data": "ap:" + e.ID + ":n"},
				}}}
				var msgs []sent
				for _, c := range t.Chats {
					if id := t.send(ctx, c, text, kb); id != 0 {
						msgs = append(msgs, sent{c, id, text})
					}
				}
				t.mu.Lock()
				t.approvMs[e.ID] = msgs
				t.mu.Unlock()
			case events.ApprovalResolved:
				t.mu.Lock()
				msgs := t.approvMs[e.ID]
				delete(t.approvMs, e.ID)
				t.mu.Unlock()
				verdict := "✅ approved"
				if !e.OK {
					verdict = "❌ denied"
				}
				verdict += " by " + html.EscapeString(e.By)
				for _, m := range msgs {
					_ = t.api(ctx, "editMessageText", map[string]any{
						"chat_id": m.chat, "message_id": m.msg, "parse_mode": "HTML",
						"text": m.text + "\n" + verdict,
					}, nil)
				}
			case events.Incident:
				head := "🚨 <b>Incident " + html.EscapeString(e.ID) + "</b>"
				if e.By == "agent" {
					head = "🩺 <b>Diagnosis " + html.EscapeString(e.ID) + "</b>"
				}
				for _, part := range split(e.Text, chunk) {
					for _, c := range t.Chats {
						t.send(ctx, c, head+"\n"+html.EscapeString(part), nil)
					}
				}
			}
		}
	}
}

// send posts an HTML message and returns its id (0 on failure).
func (t *Telegram) send(ctx context.Context, chat int64, text string, markup any) int {
	req := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "link_preview_options": map[string]bool{"is_disabled": true}}
	if markup != nil {
		req["reply_markup"] = markup
	}
	var m struct {
		MessageID int `json:"message_id"`
	}
	if err := t.api(ctx, "sendMessage", req, &m); err != nil {
		slog.Warn("telegram send", "chat", chat, "err", err)
		return 0
	}
	return m.MessageID
}

// split breaks text into chunks at line boundaries where possible.
func split(s string, n int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{"(empty)"}
	}
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut < n/2 {
			cut = n
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], "\n")
	}
	return append(out, s)
}

func clip(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
