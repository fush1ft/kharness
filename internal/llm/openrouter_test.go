package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The fallback chain must skip models that can never work, not sleep on 429, and stick to the model that answered.
func TestFallbackStickyAndDead(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		hits[req.Model]++
		mu.Unlock()
		switch req.Model {
		case "forbidden":
			http.Error(w, `{"error":{"message":"not for you"}}`, 403)
		case "limited":
			http.Error(w, `{"error":{"message":"rate limited"}}`, 429)
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		}
	}))
	defer srv.Close()
	c := New("k", []string{"forbidden", "limited", "good"})
	c.BaseURL = srv.URL
	fallbacks := 0
	c.OnFallback = func(string, error) { fallbacks++ }
	for range 3 {
		_, model, err := c.Chat(context.Background(), nil, nil)
		if err != nil || model != "good" {
			t.Fatalf("model=%q err=%v", model, err)
		}
	}
	if hits["forbidden"] != 1 || hits["limited"] != 1 || hits["good"] != 3 || fallbacks != 2 {
		t.Fatalf("hits=%v fallbacks=%d", hits, fallbacks)
	}

	all := New("k", []string{"forbidden"})
	all.BaseURL = srv.URL
	if _, _, err := all.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := all.Chat(context.Background(), nil, nil); err == nil || err.Error() != "openrouter: every configured model is unavailable for this API key" {
		t.Fatalf("dead chain error: %v", err)
	}
}
