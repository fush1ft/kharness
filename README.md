# kharness

An AI agent harness for running the single-node k3s cluster on **server1**. You talk to it from a terminal UI or
Telegram. It investigates with typed client-go tools, and it changes the cluster only after you approve a dry-run
diff. A watcher reports broken pods and nodes (plus Alertmanager alerts) and lets the agent diagnose them. The LLM
comes from **free OpenRouter models**, with a fallback chain.

```
laptop                               server1 (k3s)
┌──────────────┐  WebSocket+token   ┌──────────────────────── namespace kharness ───────────────┐
│ kharness tui │ ─── Traefik ──────▶│ kharness serve                                            │
└──────────────┘  /kharness/ws      │  ├ sessions (tui, tg:<chat>, incident) → agent loop → tools │
┌──────────────┐  Bot API polling   │  ├ approval gate ── every change waits for y/n            │
│  Telegram    │ ◀─────────────────▶│  ├ watcher (pod/node informers) + :9094 /alertmanager     │
└──────────────┘                    │  └ audit log (SQLite on PVC)                              │
                                    │ registry (127.0.0.1:5000 on the node, for own images)     │
                                    └───────────────────────────────────────────────────────────┘
```

## Tools the agent has

| Risk | Tools |
|---|---|
| read | `cluster_overview`, `list_resources` (any kind incl. CRDs), `get_resource` (Secret values redacted), `pod_logs`, `list_events`, `top`, `helm_list`, `helm_status`, `prometheus_query`, `loki_query`, `get_recipe` |
| write | `apply_manifest` (server-side apply, preview = dry-run diff), `scale`, `rollout_restart`, `helm_upgrade_install` (preview = rendered resources / manifest diff), `helm_rollback` |
| dangerous | `delete_resource`, `helm_uninstall` |
| by verb | `kubectl` fallback: read verbs run freely. `apply/patch/label/cordon/rollout undo…` count as write and `delete/drain` as dangerous. `exec`, `port-forward`, `edit`, `proxy`, `config` and the context/credential flags are refused. Secret contents are never sent to the model. |

Guardrails that hold regardless of approval:
- **Protected namespaces** (`KHARNESS_PROTECTED_NS`, default `kube-system,kube-public,kube-node-lease,kharness`) can't
  be written to.
- Writes across all namespaces (`-A`) are refused.
- Every question, tool call, approval and incident is written to the audit log (`/audit`, `/changes`).
- An approval is denied after 15 minutes without an answer.

Recipes (`get_recipe`) are step-by-step guides for setting the cluster up: `monitoring`
(kube-prometheus-stack with Alertmanager sending to kharness), `loki` (Loki and Alloy), `cert-manager` (with a
private CA) and `app` (Deployment, Service and Ingress on Traefik). For example, ask *"nainstaluj monitoring podle
receptu"* and approve the Helm install.

## Using it

```sh
make build                      # bin/kharness (static)
./bin/kharness models           # free OpenRouter models with tool calling

# Without deploying: the agent runs on the laptop with your kubeconfig
export OPENROUTER_API_KEY=sk-or-...
./bin/kharness --kubeconfig ~/.kube/server1.yaml tui --local          # TUI, approvals in the dialog
./bin/kharness --kubeconfig ~/.kube/server1.yaml chat "co je rozbité?"  # line mode, approvals as y/N
./bin/kharness --read-only ... chat                                    # never asks, never changes

# In the cluster
export OPENROUTER_API_KEY=sk-or-... TELEGRAM_BOT_TOKEN=123:abc   # Telegram is optional
export KHARNESS_URL=http://<node>/kharness                       # what the TUI connects to (default http://server1/kharness)
make deploy BOOTSTRAP_FLAGS="--telegram-chats 123456789"         # build → image → push → bootstrap
./bin/kharness tui                                               # connects using ~/.config/kharness/client.json
make deploy-dry                                                  # show what bootstrap would change
```

`make deploy` does the following:
1. Builds the image.
2. Pushes it through an ssh tunnel into the node-local registry. Nobody on server1 needs Docker or sudo.
3. Runs `kharness bootstrap`, which applies `internal/deploy/manifests.yaml`, creates or updates the secret (it
   generates the API token on the first install and keeps it afterwards), restarts the pod, waits for it to be
   ready, and writes the TUI config.

Later deploys keep the secrets you don't pass again.

**Telegram:** create a bot with @BotFather, deploy with `TELEGRAM_BOT_TOKEN`, and write to the bot. It replies with
the chat id when the chat isn't allowed. Then redeploy with `--telegram-chats <id>`. In the chat:
- plain messages are questions
- the inline buttons approve changes, and those buttons also show up for changes the TUI or incidents asked for
- `/reset`, `/cancel` and `/audit` work as commands

**TUI keys:**
- `enter` sends, `esc` cancels the running question, `pgup`/`pgdn` scroll
- `ctrl+a` opens pending approvals; in the dialog `y`/`n` decides, `j`/`k` scrolls and `esc` closes it
- commands: `/reset`, `/cancel`, `/audit`, `/changes`, `/clear`, `/quit`

## Configuration (environment of `serve`; `chat` and `tui --local` read the same)

| Variable | Default | |
|---|---|---|
| `OPENROUTER_API_KEY` | (required) | |
| `KHARNESS_MODELS` | auto: 5 free tool-capable models | comma-separated fallback chain |
| `KHARNESS_TOKEN` | (required for serve) | WebSocket API token |
| `KHARNESS_READ_ONLY` | false | |
| `KHARNESS_PROTECTED_NS` | kube-system,kube-public,kube-node-lease,kharness | |
| `KHARNESS_DATA` | `/data` in the pod, `~/.local/state/kharness` locally | audit.db |
| `KHARNESS_WATCH` / `KHARNESS_AUTODIAGNOSE` | true / true | the local commands never watch unless `tui --local --watch` |
| `KHARNESS_MAX_DIAG_PER_HOUR` | 6 | protects the free-tier rate limit |
| `KHARNESS_PROMETHEUS_URL` / `KHARNESS_LOKI_URL` | the in-cluster services from the recipes | |
| `KHARNESS_ADDR` / `KHARNESS_ALERT_ADDR` / `KHARNESS_BASE_PATH` | :8080 / :9094 / /kharness | the alert port isn't in the Ingress |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_IDS` | | chat ids are the allowlist and the recipients of incidents |

## Development

`go test ./...` runs everything offline. It includes the agent loop, the approval gate, the WebSocket flow (with a
scripted model), the Telegram flow (against a fake Bot API), the watcher detectors and webhook, the kubectl
classifier, and the TUI model. The integration tests talk to the real cluster but change nothing (reads,
server-side dry-runs, and a client-only Helm dry-run):

```sh
KHARNESS_IT_KUBECONFIG=~/.kube/server1.yaml go test -run Integration -v ./internal/tools/...
```

The layout is `internal/{llm,agent,tools/{k8s,helm,shell,observe},approval,events,session,server,client,tui,chat,watch,audit,recipes,deploy,app}`.
`internal/app` wires everything together.
