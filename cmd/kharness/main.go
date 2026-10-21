package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/fush1ft/kharness/internal/app"
	"github.com/fush1ft/kharness/internal/approval"
	"github.com/fush1ft/kharness/internal/client"
	"github.com/fush1ft/kharness/internal/deploy"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/llm"
	"github.com/fush1ft/kharness/internal/server"
	"github.com/fush1ft/kharness/internal/tools/k8s"
	"github.com/fush1ft/kharness/internal/tui"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

type globalFlags struct {
	kubeconfig string
	models     []string
	readOnly   bool
}

func rootCmd() *cobra.Command {
	var g globalFlags
	root := &cobra.Command{
		Use:          "kharness",
		Short:        "AI agent harness for managing a k3s cluster",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&g.kubeconfig, "kubeconfig", "", "kubeconfig path (default: $KUBECONFIG, ~/.kube/config, in-cluster)")
	root.PersistentFlags().StringSliceVarP(&g.models, "model", "m", nil,
		"OpenRouter model(s) in fallback order (default: $KHARNESS_MODELS, else auto-discovered free tool-capable models)")
	root.PersistentFlags().BoolVar(&g.readOnly, "read-only", false, "refuse every change (no approvals asked)")
	root.AddCommand(chatCmd(&g), tuiCmd(&g), serveCmd(), bootstrapCmd(&g), modelsCmd())
	return root
}

// localConfig is the config for commands that run the agent on this machine.
func localConfig(g *globalFlags) (app.Config, error) {
	cfg, err := app.FromEnv()
	if err != nil {
		return cfg, err
	}
	if g.kubeconfig != "" {
		cfg.Kubeconfig = g.kubeconfig
	}
	if len(g.models) > 0 {
		cfg.Models = g.models
	}
	cfg.ReadOnly = cfg.ReadOnly || g.readOnly
	if cfg.DataDir == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			cfg.DataDir = filepath.Join(dir, ".local", "state", "kharness")
		}
	}
	cfg.Watch = false
	cfg.TelegramToken = ""
	return cfg, nil
}

func chatCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "chat [question]",
		Short: "Line-based chat on this machine (one-shot with an argument, interactive without)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM)
			defer stop()
			cfg, err := localConfig(g)
			if err != nil {
				return err
			}
			rt, err := app.Build(ctx, cfg)
			if err != nil {
				return err
			}
			defer rt.Close()
			fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m\n", rt.Core.Info)

			in := bufio.NewScanner(os.Stdin)
			go printEvents(rt, "cli", in)

			ask := func(q string) {
				// Ctrl-C cancels the running question, not the program.
				qctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
				defer cancel()
				start := time.Now()
				ans, err := rt.Sessions.Ask(qctx, "cli", "cli", q, false)
				if err != nil {
					fmt.Fprintf(os.Stderr, "\033[31merror: %v\033[0m\n", err)
					return
				}
				fmt.Println(strings.TrimSpace(ans))
				fmt.Fprintf(os.Stderr, "\033[2m(%s)\033[0m\n", time.Since(start).Round(100*time.Millisecond))
			}
			if len(args) > 0 {
				ask(strings.Join(args, " "))
				return nil
			}
			fmt.Fprintln(os.Stderr, "Type a question; /reset clears the conversation, Ctrl-C cancels, Ctrl-D quits.")
			for {
				fmt.Fprint(os.Stderr, "\n\033[1m›\033[0m ")
				if !in.Scan() {
					fmt.Fprintln(os.Stderr)
					return nil
				}
				switch line := strings.TrimSpace(in.Text()); line {
				case "":
				case "/reset":
					_ = rt.Sessions.Reset("cli")
					fmt.Fprintln(os.Stderr, "conversation cleared")
				default:
					ask(line)
				}
			}
		},
	}
}

// printEvents shows tool calls and asks for approvals on the terminal. The main loop is blocked inside Ask while
// an approval is pending, so reading stdin here does not race it.
func printEvents(rt *app.Runtime, sess string, in *bufio.Scanner) {
	ch, _ := rt.Bus.Subscribe(128)
	for e := range ch {
		if e.Session != sess {
			continue
		}
		switch e.Type {
		case events.ToolCall:
			risk := ""
			if e.Risk != "read" {
				risk = " \033[33m[" + e.Risk + "]\033[0m"
			}
			fmt.Fprintf(os.Stderr, "\033[36m→ %s\033[0m%s \033[2m%s\033[0m\n", e.Tool, risk, oneLine(e.Args, 120))
		case events.ToolResult:
			if e.Error != "" {
				fmt.Fprintf(os.Stderr, "\033[31m  ✗ %s\033[0m\n", oneLine(e.Error, 200))
			}
		case events.Approval:
			body := e.Preview
			if body == "" {
				body = e.Args
			}
			fmt.Fprintf(os.Stderr, "\n\033[35;1mApprove %s (%s)?\033[0m\n%s\n\033[35mapprove? [y/N] \033[0m", e.Tool, e.Risk, colorDiff(body))
			ok := in.Scan() && strings.EqualFold(strings.TrimSpace(in.Text()), "y")
			d := approval.Decision{OK: ok, By: "cli"}
			if !ok {
				d.Reason = "denied at the terminal"
			}
			_ = rt.Gate.Resolve(e.ID, d)
		}
	}
}

func tuiCmd(g *globalFlags) *cobra.Command {
	var url, token, sess string
	var local, watch bool
	c := &cobra.Command{
		Use:   "tui",
		Short: "Terminal UI; connects to the in-cluster server (or runs the agent here with --local)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var be server.Backend
			if local {
				cfg, err := localConfig(g)
				if err != nil {
					return err
				}
				cfg.Watch = watch
				ctx, cancel := context.WithCancel(cmd.Context())
				defer cancel()
				rt, err := app.Build(ctx, cfg)
				if err != nil {
					return err
				}
				defer rt.Close()
				slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // logs would corrupt the screen
				rt.Start(ctx, func(string, error) {})
				be = server.NewLocal(rt.Core, sess, "tui")
			} else {
				cfg, _ := client.LoadConfig()
				if url != "" {
					cfg.URL = url
				}
				if token != "" {
					cfg.Token = token
				}
				if t := os.Getenv("KHARNESS_TOKEN"); cfg.Token == "" && t != "" {
					cfg.Token = t
				}
				if cfg.URL == "" || cfg.Token == "" {
					return fmt.Errorf("no server configured: run `kharness bootstrap`, pass --url/--token, or use --local (config: %s)", client.ConfigPath())
				}
				r, err := client.Dial(cfg, sess, "tui")
				if err != nil {
					return err
				}
				be = r
			}
			defer be.Close()
			if !lipgloss.HasDarkBackground() {
				tui.GlamourStyle = "light"
			}
			_, err := tea.NewProgram(tui.New(be, sess), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
			return err
		},
	}
	c.Flags().StringVar(&url, "url", "", "server URL, e.g. http://server1/kharness (default from client config)")
	c.Flags().StringVar(&token, "token", "", "API token (default from client config or $KHARNESS_TOKEN)")
	c.Flags().StringVar(&sess, "session", "tui", "conversation name (several TUIs can share one)")
	c.Flags().BoolVar(&local, "local", false, "run the agent in this process with your kubeconfig instead of connecting to the server")
	c.Flags().BoolVar(&watch, "watch", false, "with --local: also run the cluster watcher")
	return c
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the agent server (in the cluster): WebSocket API, Telegram bot, watcher, Alertmanager webhook",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			cfg, err := app.FromEnv()
			if err != nil {
				return err
			}
			if cfg.Token == "" {
				return errors.New("KHARNESS_TOKEN must be set")
			}
			rt, err := app.Build(ctx, cfg)
			if err != nil {
				return err
			}
			defer rt.Close()
			slog.Info("kharness serving", "info", rt.Core.Info, "addr", cfg.Addr, "base", cfg.BasePath,
				"telegram", rt.Telegram != nil, "watch", rt.Watcher != nil)
			rt.Start(ctx, func(component string, err error) {
				if err != nil {
					slog.Error("component stopped", "component", component, "err", err)
				}
			})

			alerts := http.NewServeMux()
			if rt.Watcher != nil {
				alerts.Handle("POST /alertmanager", rt.Watcher.AlertHandler())
			}
			errc := make(chan error, 2)
			go func() { errc <- server.Serve(ctx, cfg.Addr, rt.Core.Handler(cfg.BasePath, cfg.Token)) }()
			go func() { errc <- server.Serve(ctx, cfg.AlertAddr, alerts) }()
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				return nil
			}
		},
	}
}

func bootstrapCmd(g *globalFlags) *cobra.Command {
	var o deploy.Options
	var url string
	c := &cobra.Command{
		Use:   "bootstrap",
		Short: "Install or update kharness in the cluster and save the TUI connection config",
		Long: "Applies the namespace, node-local registry, RBAC, config, secret, deployment, service and ingress, " +
			"then waits for the pod. Reads OPENROUTER_API_KEY and (optionally) TELEGRAM_BOT_TOKEN from the environment; " +
			"on updates, secrets that are not given are kept. Push the image first (make push).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			kube, err := k8s.Load(g.kubeconfig)
			if err != nil {
				return err
			}
			o.Models = g.models
			o.OpenRouterKey = os.Getenv("OPENROUTER_API_KEY")
			o.TelegramToken = os.Getenv("TELEGRAM_BOT_TOKEN")
			res, err := deploy.Run(cmd.Context(), kube, o, func(s string) { fmt.Fprintln(os.Stderr, "• "+s) })
			fmt.Print(res.Applied)
			if err != nil {
				return err
			}
			if o.DryRun {
				return nil
			}
			if err := client.SaveConfig(client.Config{URL: url, Token: res.Token}); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ kharness is running. TUI config saved to %s — start it with `kharness tui`.\n", client.ConfigPath())
			return nil
		},
	}
	c.Flags().StringVar(&o.Image, "image", "localhost:5000/kharness:latest", "image as the node pulls it")
	c.Flags().StringVar(&o.BasePath, "base-path", "/kharness", "ingress path prefix")
	c.Flags().StringVar(&url, "url", cmp.Or(os.Getenv("KHARNESS_URL"), "http://server1/kharness"), "URL the TUI uses to reach the server (default $KHARNESS_URL)")
	c.Flags().StringSliceVar(&o.TelegramChats, "telegram-chats", nil, "allowed Telegram chat ids")
	c.Flags().BoolVar(&o.DryRun, "dry-run", false, "only show what would change")
	return c
}

func modelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List free OpenRouter models that support tool calling",
		RunE: func(cmd *cobra.Command, _ []string) error {
			found, err := llm.New("", nil).FreeToolModels(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "MODEL\tCONTEXT")
			for _, m := range found {
				fmt.Fprintf(w, "%s\t%d\n", m.ID, m.ContextLength)
			}
			return w.Flush()
		},
	}
}

func colorDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			lines[i] = "\033[36m" + l + "\033[0m"
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, "  + "):
			lines[i] = "\033[32m" + l + "\033[0m"
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, "  - "):
			lines[i] = "\033[31m" + l + "\033[0m"
		}
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
