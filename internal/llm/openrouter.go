// Package llm is a minimal OpenRouter client (OpenAI-compatible chat completions with tool calling).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const DefaultBaseURL = "https://openrouter.ai/api/v1"

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Client struct {
	BaseURL string
	APIKey  string
	// Models is the fallback chain: on rate limits or upstream errors the next one is tried.
	Models []string
	HTTP   *http.Client
	// OnFallback, if set, is called when a model fails and the next one is tried.
	OnFallback func(model string, err error)

	mu   sync.Mutex
	next int             // index of the model that answered last; tried first
	dead map[string]bool // models that can never work for this key (401/403/404)
}

func New(apiKey string, models []string) *Client {
	// A free model that is still silent after a minute is better skipped than waited for.
	return &Client{BaseURL: DefaultBaseURL, APIKey: apiKey, Models: models, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// StatusError is a non-2xx response from the API.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("openrouter: HTTP %d: %s", e.Status, e.Body) }

// Chat sends the conversation to the first model in the chain that answers. It returns the reply and the model used.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []ToolDef) (Message, string, error) {
	if len(c.Models) == 0 {
		return Message{}, "", errors.New("openrouter: no models configured")
	}
	c.mu.Lock()
	start, n := c.next, len(c.Models)
	c.mu.Unlock()
	var lastErr error
	for k := range n {
		i := (start + k) % n
		model := c.Models[i]
		c.mu.Lock()
		skip := c.dead[model]
		c.mu.Unlock()
		if skip {
			continue
		}
		msg, err := c.chatOnce(ctx, model, msgs, tools)
		if err == nil {
			c.mu.Lock()
			c.next = i // sticky: later steps start with the model that works
			c.mu.Unlock()
			return msg, model, nil
		}
		if ctx.Err() != nil {
			return Message{}, model, ctx.Err()
		}
		var se *StatusError
		if errors.As(err, &se) && (se.Status == 401 || se.Status == 403 || se.Status == 404) {
			c.mu.Lock()
			if c.dead == nil {
				c.dead = map[string]bool{}
			}
			c.dead[model] = true
			c.mu.Unlock()
		}
		lastErr = fmt.Errorf("%s: %w", model, err)
		if c.OnFallback != nil {
			c.OnFallback(model, err)
		}
	}
	if lastErr == nil {
		return Message{}, "", errors.New("openrouter: every configured model is unavailable for this API key")
	}
	return Message{}, "", fmt.Errorf("all models failed, last error: %w", lastErr)
}

func (c *Client) chatOnce(ctx context.Context, model string, msgs []Message, tools []ToolDef) (Message, error) {
	body, err := json.Marshal(chatRequest{Model: model, Messages: msgs, Tools: tools})
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("X-Title", "kharness")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// 429 included: the next model in the chain is the retry.
	if resp.StatusCode/100 != 2 {
		return Message{}, &StatusError{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Message{}, fmt.Errorf("decode response: %w", err)
	}
	// OpenRouter can report upstream errors inside a 200 body.
	if cr.Error != nil {
		return Message{}, fmt.Errorf("upstream error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Message{}, errors.New("empty response (no choices)")
	}
	m := cr.Choices[0].Message
	if m.Role == "" {
		m.Role = "assistant"
	}
	return m, nil
}

// ModelInfo is the subset of /models we care about.
type ModelInfo struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
}

// FreeToolModels lists free models that advertise tool-calling support, largest context first.
func (c *Client) FreeToolModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, &StatusError{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var free []ModelInfo
	for _, m := range out.Data {
		if strings.HasSuffix(m.ID, ":free") && slices.Contains(m.SupportedParameters, "tools") {
			free = append(free, m)
		}
	}
	slices.SortStableFunc(free, func(a, b ModelInfo) int { return b.ContextLength - a.ContextLength })
	return free, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
