// Package shell is the kubectl escape hatch for things the typed tools don't cover. Verbs are allowlisted, the
// risk is derived from the verb, and credentials/context flags are refused.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/fush1ft/kharness/internal/tools"
)

var (
	readVerbs = []string{"get", "describe", "logs", "top", "explain", "api-resources", "api-versions", "version",
		"cluster-info", "events", "wait", "kustomize"}
	writeVerbs = []string{"apply", "create", "patch", "label", "annotate", "scale", "set", "cordon", "uncordon",
		"taint", "autoscale", "expose", "replace"}
	dangerousVerbs = []string{"delete", "drain"}
	// Flags that would change identity/target cluster or leak credentials.
	deniedFlags = []string{"--kubeconfig", "--context", "--cluster", "--user", "--token", "--as", "--as-group",
		"--as-uid", "--server", "-s", "--certificate-authority", "--client-certificate", "--client-key",
		"--insecure-skip-tls-verify", "--username", "--password"}
	// Verbs that support --dry-run=server for the approval preview.
	dryRunVerbs = []string{"apply", "create", "patch", "label", "annotate", "scale", "set", "cordon", "uncordon",
		"taint", "autoscale", "expose", "replace", "delete", "drain", "rollout"}
)

type Kubectl struct {
	// Kubeconfig, if set, is exported as KUBECONFIG; otherwise kubectl uses its defaults / in-cluster config.
	Kubeconfig string
	Protected  []string
	Timeout    time.Duration
}

type args struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func (k *Kubectl) Name() string { return "kubectl" }
func (k *Kubectl) Description() string {
	return "Run kubectl for anything the typed tools don't cover (e.g. rollout history/undo, cordon, drain, taint, " +
		"label). Pass arguments without the leading 'kubectl'. Read verbs run freely; changes need approval. exec, " +
		"port-forward, edit and context/credential flags are not allowed. Prefer the typed tools when they fit."
}
func (k *Kubectl) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"args": {"type":"array","items":{"type":"string"},"description":"e.g. [\"rollout\",\"history\",\"deploy/web\",\"-n\",\"apps\"]"},
		"stdin": {"type":"string","description":"Piped to kubectl, for use with -f -"}
	},"required":["args"]}`)
}
func (k *Kubectl) Risk() tools.Risk { return tools.Dangerous }

// RiskFor classifies by verb; invalid calls are rated Read so they reach Run and fail there with a clear error
// instead of bothering the operator with an approval.
func (k *Kubectl) RiskFor(raw json.RawMessage) tools.Risk {
	a, err := parse(raw)
	if err != nil {
		return tools.Read
	}
	risk, err := classify(a.Args)
	if err != nil {
		return tools.Read
	}
	return risk
}

func (k *Kubectl) Preview(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := k.validate(raw)
	if err != nil {
		return "", err
	}
	cmdline := "kubectl " + strings.Join(a.Args, " ")
	if !slices.Contains(dryRunVerbs, verbOf(a.Args)) || verbOf(a.Args) == "rollout" && !slices.Contains(a.Args, "restart") && !slices.Contains(a.Args, "undo") {
		return cmdline, nil
	}
	out, err := k.run(ctx, append(slices.Clone(a.Args), "--dry-run=server"), a.Stdin)
	if err != nil {
		if strings.Contains(out, "unknown flag") {
			return cmdline + "\n(no dry-run available for this command)", nil
		}
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return cmdline + "\n\ndry-run:\n" + strings.TrimSpace(out), nil
}

func (k *Kubectl) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := k.validate(raw)
	if err != nil {
		return "", err
	}
	out, err := k.run(ctx, a.Args, a.Stdin)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, clip(strings.TrimSpace(out)))
	}
	if strings.TrimSpace(out) == "" {
		return "(no output)", nil
	}
	return clip(out), nil
}

func (k *Kubectl) run(ctx context.Context, argv []string, stdin string) (string, error) {
	timeout := k.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", argv...)
	cmd.Env = os.Environ()
	if k.Kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+k.Kubeconfig)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func parse(raw json.RawMessage) (args, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(a.Args) > 0 && a.Args[0] == "kubectl" {
		a.Args = a.Args[1:]
	}
	if len(a.Args) == 0 {
		return a, errors.New("args is empty")
	}
	return a, nil
}

func (k *Kubectl) validate(raw json.RawMessage) (args, error) {
	a, err := parse(raw)
	if err != nil {
		return a, err
	}
	risk, err := classify(a.Args)
	if err != nil {
		return a, err
	}
	if risk != tools.Read {
		for _, ns := range namespaces(a.Args, a.Stdin) {
			if slices.Contains(k.Protected, ns) {
				return a, fmt.Errorf("namespace %q is protected; kharness will not modify it", ns)
			}
		}
		if slices.Contains(a.Args, "-A") || slices.Contains(a.Args, "--all-namespaces") {
			return a, errors.New("changes across all namespaces are not allowed; target one namespace")
		}
	}
	if verbOf(a.Args) == "get" && touchesSecrets(a.Args) && revealsData(a.Args) {
		return a, errors.New("secret contents are never sent to the model; list secrets without -o yaml/json, or use get_resource (redacted)")
	}
	return a, nil
}

// classify returns the risk of a kubectl invocation or an error if it is not allowed.
func classify(argv []string) (tools.Risk, error) {
	for _, a := range argv {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(deniedFlags, name) {
			return 0, fmt.Errorf("flag %s is not allowed", name)
		}
	}
	verb := verbOf(argv)
	switch {
	case verb == "":
		return 0, errors.New("no kubectl verb given")
	case slices.Contains(readVerbs, verb):
		return tools.Read, nil
	case slices.Contains(writeVerbs, verb):
		return tools.Write, nil
	case slices.Contains(dangerousVerbs, verb):
		return tools.Dangerous, nil
	case verb == "rollout":
		switch sub := subVerb(argv); sub {
		case "status", "history":
			return tools.Read, nil
		case "restart", "undo", "pause", "resume":
			return tools.Write, nil
		default:
			return 0, fmt.Errorf("kubectl rollout %s is not allowed", sub)
		}
	case verb == "auth":
		if subVerb(argv) == "can-i" {
			return tools.Read, nil
		}
	}
	return 0, fmt.Errorf("kubectl %s is not allowed", verb)
}

// verbOf returns the first positional argument, skipping flags and the value of -n/--namespace.
func verbOf(argv []string) string {
	pos := positionals(argv)
	if len(pos) == 0 {
		return ""
	}
	return pos[0]
}

func subVerb(argv []string) string {
	pos := positionals(argv)
	if len(pos) < 2 {
		return ""
	}
	return pos[1]
}

func positionals(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") {
			if (a == "-n" || a == "--namespace" || a == "-o" || a == "--output" || a == "-l" || a == "--selector" || a == "-f" || a == "--filename" || a == "-c" || a == "--container") && i+1 < len(argv) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// namespaces lists the namespaces a call targets: -n values (default "default") plus any in stdin manifests.
func namespaces(argv []string, stdin string) []string {
	var out []string
	for i, a := range argv {
		switch {
		case (a == "-n" || a == "--namespace") && i+1 < len(argv):
			out = append(out, argv[i+1])
		case strings.HasPrefix(a, "--namespace="):
			out = append(out, strings.TrimPrefix(a, "--namespace="))
		case strings.HasPrefix(a, "-n="):
			out = append(out, strings.TrimPrefix(a, "-n="))
		}
	}
	if len(out) == 0 {
		out = append(out, "default")
	}
	if stdin != "" {
		dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(stdin), 4096)
		for {
			var m struct {
				Kind     string
				Metadata struct{ Name, Namespace string }
			}
			if err := dec.Decode(&m); err != nil {
				break
			}
			if m.Metadata.Namespace != "" {
				out = append(out, m.Metadata.Namespace)
			}
			if m.Kind == "Namespace" {
				out = append(out, m.Metadata.Name)
			}
		}
	}
	return out
}

func touchesSecrets(argv []string) bool {
	for _, p := range positionals(argv)[1:] {
		for _, r := range strings.Split(p, ",") {
			r, _, _ = strings.Cut(r, "/")
			if r == "secret" || r == "secrets" || strings.HasPrefix(r, "secrets.") || r == "all" {
				return r != "all" // "all" does not include secrets
			}
		}
	}
	return false
}

func revealsData(argv []string) bool {
	for i, a := range argv {
		var v string
		switch {
		case (a == "-o" || a == "--output") && i+1 < len(argv):
			v = argv[i+1]
		case strings.HasPrefix(a, "-o="), strings.HasPrefix(a, "--output="):
			_, v, _ = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			v = a[2:]
		default:
			continue
		}
		if v != "wide" && v != "name" {
			return true
		}
	}
	return false
}

func clip(s string) string {
	const n = 12000
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
