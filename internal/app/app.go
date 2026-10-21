// Package app wires configuration into a running harness (clients, tools, sessions, gate, watcher, Telegram).
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fush1ft/kharness/internal/agent"
	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/chat"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/llm"
	"github.com/fush1ft/kharness/internal/recipes"
	"github.com/fush1ft/kharness/internal/server"
	"github.com/fush1ft/kharness/internal/session"
	"github.com/fush1ft/kharness/internal/tools"
	"github.com/fush1ft/kharness/internal/tools/helm"
	"github.com/fush1ft/kharness/internal/tools/k8s"
	"github.com/fush1ft/kharness/internal/tools/observe"
	"github.com/fush1ft/kharness/internal/tools/shell"
	"github.com/fush1ft/kharness/internal/watch"
)

type Config struct {
	Kubeconfig string
	APIKey     string
	Models     []string
	ReadOnly   bool
	Protected  []string
	DataDir    string // audit DB lives here; empty = no audit log

	PrometheusURL string
	LokiURL       string

	Token     string // WebSocket API token
	Addr      string // public HTTP listener
	AlertAddr string // in-cluster Alertmanager webhook listener
	BasePath  string

	TelegramToken string
	TelegramChats []int64

	Watch          bool
	AutoDiagnose   bool
	MaxDiagPerHour int
}

// FromEnv reads the KHARNESS_* / OPENROUTER_* / TELEGRAM_* environment (used by `serve`, defaults for the rest).
func FromEnv() (Config, error) {
	c := Config{
		Kubeconfig:     os.Getenv("KUBECONFIG"),
		APIKey:         os.Getenv("OPENROUTER_API_KEY"),
		Models:         list(os.Getenv("KHARNESS_MODELS")),
		ReadOnly:       boolEnv("KHARNESS_READ_ONLY", false),
		Protected:      list(env("KHARNESS_PROTECTED_NS", "kube-system,kube-public,kube-node-lease,kharness")),
		DataDir:        os.Getenv("KHARNESS_DATA"),
		PrometheusURL:  env("KHARNESS_PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc:9090"),
		LokiURL:        env("KHARNESS_LOKI_URL", "http://loki.monitoring.svc:3100"),
		Token:          os.Getenv("KHARNESS_TOKEN"),
		Addr:           env("KHARNESS_ADDR", ":8080"),
		AlertAddr:      env("KHARNESS_ALERT_ADDR", ":9094"),
		BasePath:       env("KHARNESS_BASE_PATH", "/kharness"),
		TelegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		Watch:          boolEnv("KHARNESS_WATCH", true),
		AutoDiagnose:   boolEnv("KHARNESS_AUTODIAGNOSE", true),
		MaxDiagPerHour: intEnv("KHARNESS_MAX_DIAG_PER_HOUR", 6),
	}
	for _, s := range list(os.Getenv("TELEGRAM_CHAT_IDS")) {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return c, fmt.Errorf("TELEGRAM_CHAT_IDS: %q is not a chat id", s)
		}
		c.TelegramChats = append(c.TelegramChats, id)
	}
	return c, nil
}

// Runtime is a built harness.
type Runtime struct {
	Cfg      Config
	Kube     *k8s.Clients
	LLM      *llm.Client
	Bus      *events.Bus
	Gate     *approval.Gate
	Audit    *audit.Log
	Sessions *session.Manager
	Core     *server.Core
	Watcher  *watch.Watcher
	Telegram *chat.Telegram
}

// Build creates everything except goroutines; call Start to run the watcher and bot.
func Build(ctx context.Context, cfg Config) (*Runtime, error) {
	kube, err := k8s.Load(cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := NewLLM(ctx, cfg.APIKey, cfg.Models)
	if err != nil {
		return nil, err
	}
	var log *audit.Log
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
		if log, err = audit.Open(filepath.Join(cfg.DataDir, "audit.db")); err != nil {
			return nil, err
		}
	} else {
		log, _ = audit.Open("")
	}

	bus := events.NewBus()
	client.OnFallback = func(model string, err error) {
		bus.Publish(events.Event{Type: events.Notice, Text: fmt.Sprintf("model %s failed (%s), trying the next one", model, firstLine(err.Error(), 120))})
	}
	gate := approval.NewGate(bus)
	gate.OnDecision = func(r approval.Request, d approval.Decision) {
		log.Add(audit.Entry{Session: r.Session, Kind: "approval", Tool: r.Tool, Risk: r.Risk, Args: r.Args, OK: d.OK, Actor: d.By, Detail: d.Reason})
	}
	reg := Registry(kube, cfg)
	var approver agent.Approver = gate
	if cfg.ReadOnly {
		approver = agent.DenyAll{}
	}
	sessions := session.NewManager(bus, log, func(sess string) *agent.Agent {
		a := agent.New(client, reg, SystemPrompt(sess, cfg.ReadOnly))
		a.Approver = approver
		return a
	})
	mode := "approval required for changes"
	if cfg.ReadOnly {
		mode = "read-only"
	}
	rt := &Runtime{
		Cfg: cfg, Kube: kube, LLM: client, Bus: bus, Gate: gate, Audit: log, Sessions: sessions,
		Core: &server.Core{Sessions: sessions, Gate: gate, Bus: bus, Audit: log,
			Info: fmt.Sprintf("cluster %s · models %s · %s", kube.Config.Host, strings.Join(client.Models, ", "), mode)},
	}
	if cfg.Watch {
		rt.Watcher = &watch.Watcher{
			Client: kube.Typed, Bus: bus, Sessions: sessions, Audit: log,
			AutoDiagnose: cfg.AutoDiagnose, MaxPerHour: cfg.MaxDiagPerHour,
		}
	}
	if cfg.TelegramToken != "" {
		if len(cfg.TelegramChats) == 0 {
			return nil, errors.New("TELEGRAM_BOT_TOKEN is set but TELEGRAM_CHAT_IDS is empty (write to the bot, it replies with the chat id)")
		}
		rt.Telegram = &chat.Telegram{Token: cfg.TelegramToken, Chats: cfg.TelegramChats, Sessions: sessions, Gate: gate, Bus: bus, Audit: log}
	}
	return rt, nil
}

// Start runs the watcher and Telegram bot in the background.
func (rt *Runtime) Start(ctx context.Context, report func(component string, err error)) {
	if rt.Watcher != nil {
		go func() { report("watcher", rt.Watcher.Run(ctx)) }()
	}
	if rt.Telegram != nil {
		go func() { report("telegram", rt.Telegram.Run(ctx)) }()
	}
}

func (rt *Runtime) Close() { _ = rt.Audit.Close() }

// Registry assembles the tool set. In read-only mode tools that always change state are left out.
func Registry(kube *k8s.Clients, cfg Config) *tools.Registry {
	pol := k8s.Policy{ProtectedNamespaces: cfg.Protected}
	h := &helm.Helm{Kubeconfig: cfg.Kubeconfig, Protected: cfg.Protected}
	obs := &observe.Observe{PrometheusURL: cfg.PrometheusURL, LokiURL: cfg.LokiURL}
	all := append(k8s.ReadTools(kube), recipes.Tool())
	all = append(all, obs.Tools()...)
	all = append(all, h.Tools()...)
	all = append(all, k8s.WriteTools(kube, pol)...)
	all = append(all, &shell.Kubectl{Kubeconfig: cfg.Kubeconfig, Protected: cfg.Protected})
	reg := tools.NewRegistry()
	for _, t := range all {
		if cfg.ReadOnly && t.Risk() != tools.Read {
			if _, dynamic := t.(tools.RiskAssessor); !dynamic {
				continue
			}
		}
		reg.Add(t)
	}
	return reg
}

// NewLLM builds the OpenRouter client; with no models configured it picks the 5 free tool-capable models with
// the largest context.
func NewLLM(ctx context.Context, key string, models []string) (*llm.Client, error) {
	if key == "" {
		return nil, errors.New("OPENROUTER_API_KEY is not set")
	}
	c := llm.New(key, models)
	if len(models) == 0 {
		found, err := c.FreeToolModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover free models: %w", err)
		}
		if len(found) == 0 {
			return nil, errors.New("no free tool-capable models found; set KHARNESS_MODELS / --model")
		}
		for _, m := range found[:min(5, len(found))] {
			c.Models = append(c.Models, m.ID)
		}
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolEnv(k string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(k))
	if err != nil {
		return def
	}
	return v
}

func intEnv(k string, def int) int {
	v, err := strconv.Atoi(os.Getenv(k))
	if err != nil {
		return def
	}
	return v
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
