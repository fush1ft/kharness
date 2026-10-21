// Package client is the TUI's connection to a remote `kharness serve` over WebSocket, with auto-reconnect.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/server"
)

// Config is stored in ~/.config/kharness/client.json by `kharness bootstrap`.
type Config struct {
	URL   string `json:"url"` // e.g. http://server1/kharness
	Token string `json:"token"`
}

func ConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "kharness", "client.json")
}

func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func SaveConfig(c Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

type Remote struct {
	wsURL  string
	header http.Header

	events chan events.Event
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	conn *websocket.Conn
}

// Dial connects and keeps reconnecting in the background until Close.
func Dial(cfg Config, session, user string) (*Remote, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/ws")
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	q := u.Query()
	q.Set("session", session)
	q.Set("user", user)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithCancel(context.Background())
	r := &Remote{
		wsURL: u.String(), header: http.Header{"Authorization": {"Bearer " + cfg.Token}},
		events: make(chan events.Event, 256), ctx: ctx, cancel: cancel,
	}
	// Fail fast on the first attempt so a wrong URL/token is reported instead of retried forever.
	conn, err := r.dial()
	if err != nil {
		cancel()
		return nil, err
	}
	go r.loop(conn)
	return r, nil
}

func (r *Remote) dial() (*websocket.Conn, error) {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := d.DialContext(r.ctx, r.wsURL, r.header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("connect %s: %s", redact(r.wsURL), resp.Status)
		}
		return nil, fmt.Errorf("connect %s: %w", redact(r.wsURL), err)
	}
	return conn, nil
}

func (r *Remote) loop(conn *websocket.Conn) {
	defer close(r.events)
	for {
		r.mu.Lock()
		r.conn = conn
		r.mu.Unlock()
		for {
			var e events.Event
			if err := conn.ReadJSON(&e); err != nil {
				break
			}
			r.emit(e)
		}
		conn.Close()
		r.mu.Lock()
		r.conn = nil
		r.mu.Unlock()
		if r.ctx.Err() != nil {
			return
		}
		r.emit(events.Event{Type: events.Notice, Text: "disconnected, reconnecting…", Error: "disconnected"})
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			c, err := r.dial()
			if err == nil {
				conn = c
				break
			}
		}
	}
}

func (r *Remote) emit(e events.Event) {
	select {
	case r.events <- e:
	case <-r.ctx.Done():
	}
}

func (r *Remote) Events() <-chan events.Event { return r.events }

func (r *Remote) Send(m server.ClientMsg) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return errors.New("not connected")
	}
	_ = r.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return r.conn.WriteJSON(m)
}

func (r *Remote) Close() error {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

func redact(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}
