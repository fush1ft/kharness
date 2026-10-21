package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fush1ft/kharness/internal/events"
)

// Handler serves the public API under basePath: GET {base}/healthz and GET {base}/ws (WebSocket, token auth).
func (c *Core) Handler(basePath, token string) http.Handler {
	base := "/" + strings.Trim(basePath, "/")
	if base == "/" {
		base = ""
	}
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET "+base+"/healthz", health)
	up := websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 16384,
		// Browsers are not a client; the token is the access control.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	mux.HandleFunc("GET "+base+"/ws", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sess := r.URL.Query().Get("session")
		if sess == "" {
			sess = "tui"
		}
		if strings.HasPrefix(sess, "tg:") || sess == "incident" {
			sess = "tui:" + sess // Telegram/incident sessions are not attachable from outside
		}
		by := r.URL.Query().Get("user")
		if by == "" {
			by = "tui"
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(1 << 20)
		slog.Info("client connected", "session", sess, "by", by, "remote", r.RemoteAddr)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		// Keepalive through Traefik: ping every 30s, drop the client if pongs stop.
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(90 * time.Second)) })
		writeMu := make(chan struct{}, 1)
		write := func(f func() error) error {
			writeMu <- struct{}{}
			defer func() { <-writeMu }()
			_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			return f()
		}
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if write(func() error { return conn.WriteMessage(websocket.PingMessage, nil) }) != nil {
						cancel()
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
		err = c.Attach(ctx, sess, by,
			func(e events.Event) error { return write(func() error { return conn.WriteJSON(e) }) },
			func() (ClientMsg, error) {
				var m ClientMsg
				err := conn.ReadJSON(&m)
				return m, err
			})
		slog.Info("client disconnected", "session", sess, "err", err)
	})
	return mux
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// Serve runs an HTTP server until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(sctx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
