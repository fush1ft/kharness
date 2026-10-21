// Package helm exposes Helm releases to the agent through the Helm v3 SDK (no helm binary needed).
package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage/driver"
	"sigs.k8s.io/yaml"

	"github.com/fush1ft/kharness/internal/tools"
)

const maxOutput = 12000

type Helm struct {
	// Kubeconfig is passed to Helm; empty means default loading rules with in-cluster fallback.
	Kubeconfig string
	// Protected namespaces cannot receive releases.
	Protected []string
}

func (h *Helm) config(ns string) (*action.Configuration, *cli.EnvSettings, error) {
	settings := cli.New()
	settings.KubeConfig = h.Kubeconfig
	settings.SetNamespace(ns)
	cfg := new(action.Configuration)
	if err := cfg.Init(settings.RESTClientGetter(), ns, "secret", func(string, ...any) {}); err != nil {
		return nil, nil, err
	}
	rc, err := registry.NewClient(registry.ClientOptCredentialsFile(settings.RegistryConfig))
	if err != nil {
		return nil, nil, err
	}
	cfg.RegistryClient = rc
	return cfg, settings, nil
}

func (h *Helm) check(ns string) error {
	if ns == "" {
		return errors.New("namespace is required")
	}
	if slices.Contains(h.Protected, ns) {
		return fmt.Errorf("namespace %q is protected; kharness will not install releases into it", ns)
	}
	return nil
}

func (h *Helm) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Func{
			N: "helm_list", R: tools.Read,
			D: "List Helm releases (all namespaces unless namespace is given).",
			S: schema(`{"namespace": {"type": "string"}}`),
			F: h.list,
		},
		&tools.Func{
			N: "helm_status", R: tools.Read,
			D: "Status, revision history and user-supplied values of a Helm release.",
			S: schema(`{"release": {"type": "string"}, "namespace": {"type": "string"}}`, "release", "namespace"),
			F: h.status,
		},
		&tools.Func{
			N: "helm_upgrade_install", R: tools.Write,
			D: "Install a chart, or upgrade the release if it exists. `values` must be the COMPLETE set of overrides " +
				"(previous values are not reused; read them with helm_status first). The operator sees a dry-run summary/diff.",
			S: schema(`{
				"release": {"type": "string"},
				"chart": {"type": "string", "description": "Chart name in the repo (e.g. kube-prometheus-stack) or an oci:// reference"},
				"repo_url": {"type": "string", "description": "Chart repository URL (not needed for oci://)"},
				"version": {"type": "string", "description": "Chart version; empty = latest"},
				"namespace": {"type": "string"},
				"values": {"type": "string", "description": "Values as YAML"},
				"create_namespace": {"type": "boolean", "description": "Default true"}
			}`, "release", "chart", "namespace"),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return h.upgradeInstall(ctx, raw, false)
			},
			P: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return h.upgradeInstall(ctx, raw, true)
			},
		},
		&tools.Func{
			N: "helm_rollback", R: tools.Write,
			D: "Roll a release back to an earlier revision (0 = previous).",
			S: schema(`{"release": {"type": "string"}, "namespace": {"type": "string"}, "revision": {"type": "integer"}}`, "release", "namespace"),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return h.rollback(raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return h.rollback(raw, true) },
		},
		&tools.Func{
			N: "helm_uninstall", R: tools.Dangerous,
			D: "Uninstall a Helm release and delete its resources.",
			S: schema(`{"release": {"type": "string"}, "namespace": {"type": "string"}}`, "release", "namespace"),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return h.uninstall(raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return h.uninstall(raw, true) },
		},
	}
}

func (h *Helm) list(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Namespace string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cfg, _, err := h.config(a.Namespace)
	if err != nil {
		return "", err
	}
	l := action.NewList(cfg)
	l.AllNamespaces = a.Namespace == ""
	l.All = true
	rels, err := l.Run()
	if err != nil {
		return "", err
	}
	if len(rels) == 0 {
		return "No Helm releases.", nil
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tRELEASE\tREV\tSTATUS\tCHART\tAPP\tUPDATED")
	for _, r := range rels {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s-%s\t%s\t%s\n", r.Namespace, r.Name, r.Version, r.Info.Status,
			r.Chart.Metadata.Name, r.Chart.Metadata.Version, r.Chart.Metadata.AppVersion, r.Info.LastDeployed.Format("2006-01-02 15:04"))
	}
	w.Flush()
	return b.String(), nil
}

func (h *Helm) status(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Release, Namespace string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cfg, _, err := h.config(a.Namespace)
	if err != nil {
		return "", err
	}
	hist, err := action.NewHistory(cfg).Run(a.Release)
	if err != nil {
		return "", err
	}
	releaseutil.SortByRevision(hist)
	var b strings.Builder
	cur := hist[len(hist)-1]
	fmt.Fprintf(&b, "Release %s/%s: revision %d, status %s, chart %s-%s\n", cur.Namespace, cur.Name, cur.Version,
		cur.Info.Status, cur.Chart.Metadata.Name, cur.Chart.Metadata.Version)
	b.WriteString("\nHistory:\n")
	for _, r := range hist[max(0, len(hist)-10):] {
		fmt.Fprintf(&b, "  rev %d  %s  %s  %s\n", r.Version, r.Info.LastDeployed.Format("2006-01-02 15:04"), r.Info.Status, r.Info.Description)
	}
	vals, _ := yaml.Marshal(cur.Config)
	fmt.Fprintf(&b, "\nUser-supplied values:\n%s\n", strings.TrimSpace(string(vals)))
	if cur.Info.Notes != "" {
		fmt.Fprintf(&b, "\nNotes:\n%s\n", clip(cur.Info.Notes, 3000))
	}
	return clip(b.String(), maxOutput), nil
}

type installArgs struct {
	Release, Chart, Namespace, Version, Values string
	RepoURL                                    string `json:"repo_url"`
	CreateNamespace                            *bool  `json:"create_namespace"`
}

func (h *Helm) upgradeInstall(ctx context.Context, raw json.RawMessage, dryRun bool) (string, error) {
	var a installArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if a.Release == "" || a.Chart == "" {
		return "", errors.New("release and chart are required")
	}
	if err := h.check(a.Namespace); err != nil {
		return "", err
	}
	vals := map[string]any{}
	if strings.TrimSpace(a.Values) != "" {
		if err := yaml.Unmarshal([]byte(a.Values), &vals); err != nil {
			return "", fmt.Errorf("values: %w", err)
		}
	}
	cfg, settings, err := h.config(a.Namespace)
	if err != nil {
		return "", err
	}

	// Locate and load the chart (downloads into the Helm cache).
	inst := action.NewInstall(cfg)
	inst.RepoURL, inst.Version = a.RepoURL, a.Version
	inst.SetRegistryClient(cfg.RegistryClient)
	path, err := inst.LocateChart(a.Chart, settings)
	if err != nil {
		return "", fmt.Errorf("locate chart: %w", err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return "", fmt.Errorf("load chart: %w", err)
	}

	hist, err := action.NewHistory(cfg).Run(a.Release)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return "", err
	}
	var prev *release.Release
	if len(hist) > 0 {
		releaseutil.SortByRevision(hist)
		prev = hist[len(hist)-1]
	}
	// A release whose only revision failed to install must be reinstalled, not upgraded.
	upgrade := prev != nil && !(len(hist) == 1 && prev.Info.Status == release.StatusFailed)

	var rel *release.Release
	if upgrade {
		up := action.NewUpgrade(cfg)
		up.Namespace, up.Version, up.RepoURL = a.Namespace, a.Version, a.RepoURL
		up.Timeout = 10 * time.Minute
		up.MaxHistory = 10
		if dryRun {
			up.DryRun, up.DryRunOption = true, "server"
		}
		rel, err = up.RunWithContext(ctx, a.Release, ch, vals)
	} else {
		inst.ReleaseName, inst.Namespace = a.Release, a.Namespace
		inst.CreateNamespace = a.CreateNamespace == nil || *a.CreateNamespace
		inst.Timeout = 10 * time.Minute
		inst.Replace = prev != nil
		if dryRun {
			// Client-side: the namespace (and CRDs) may not exist yet, so a server dry-run would fail.
			inst.DryRun, inst.DryRunOption, inst.ClientOnly = true, "client", true
			// ClientOnly would assume Kubernetes v1.20 and reject charts with a newer kubeVersion constraint.
			dc, err := settings.RESTClientGetter().ToDiscoveryClient()
			if err != nil {
				return "", err
			}
			sv, err := dc.ServerVersion()
			if err != nil {
				return "", err
			}
			if inst.KubeVersion, err = chartutil.ParseKubeVersion(sv.GitVersion); err != nil {
				return "", err
			}
		}
		rel, err = inst.RunWithContext(ctx, ch, vals)
	}
	if err != nil {
		return "", err
	}

	verb := map[bool]string{true: "upgrade", false: "install"}[upgrade]
	if !dryRun {
		out := fmt.Sprintf("%sed %s/%s → revision %d, status %s (chart %s)", strings.TrimSuffix(verb, "e"), rel.Namespace,
			rel.Name, rel.Version, rel.Info.Status, chartRef(ch))
		if rel.Info.Notes != "" {
			out += "\n\nNotes:\n" + clip(rel.Info.Notes, 2000)
		}
		return out, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s/%s with chart %s\n", strings.ToUpper(verb), a.Namespace, a.Release, chartRef(ch))
	if prev != nil {
		fmt.Fprintf(&b, "current: revision %d, chart %s, status %s\n", prev.Version, chartRef(prev.Chart), prev.Info.Status)
	}
	if len(vals) > 0 {
		y, _ := yaml.Marshal(vals)
		fmt.Fprintf(&b, "\nvalues:\n%s", indent(string(y)))
	}
	newRes := resources(rel.Manifest)
	if prev == nil {
		fmt.Fprintf(&b, "\nresources (%d):\n", len(newRes))
		for _, r := range sortedKeys(newRes) {
			fmt.Fprintf(&b, "  + %s\n", r)
		}
		if len(ch.CRDObjects()) > 0 {
			fmt.Fprintf(&b, "  + %d CustomResourceDefinitions\n", len(ch.CRDObjects()))
		}
		return clip(b.String(), maxOutput), nil
	}
	oldRes := resources(prev.Manifest)
	b.WriteString("\nresource changes:\n")
	changed := 0
	for _, r := range sortedKeys(newRes, oldRes) {
		o, inOld := oldRes[r]
		n, inNew := newRes[r]
		switch {
		case !inOld:
			fmt.Fprintf(&b, "  + %s\n", r)
		case !inNew:
			fmt.Fprintf(&b, "  - %s\n", r)
		case o != n:
			fmt.Fprintf(&b, "  ~ %s\n", r)
		default:
			continue
		}
		changed++
	}
	if changed == 0 {
		b.WriteString("  (none)\n")
	}
	d, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(prev.Manifest), B: difflib.SplitLines(rel.Manifest), FromFile: "current", ToFile: "new", Context: 2,
	})
	if d != "" {
		fmt.Fprintf(&b, "\nmanifest diff:\n%s", d)
	}
	return clip(b.String(), maxOutput), nil
}

func (h *Helm) rollback(raw json.RawMessage, dryRun bool) (string, error) {
	var a struct {
		Release, Namespace string
		Revision           int
	}
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if err := h.check(a.Namespace); err != nil {
		return "", err
	}
	cfg, _, err := h.config(a.Namespace)
	if err != nil {
		return "", err
	}
	hist, err := action.NewHistory(cfg).Run(a.Release)
	if err != nil {
		return "", err
	}
	releaseutil.SortByRevision(hist)
	cur := hist[len(hist)-1]
	target := a.Revision
	if target == 0 {
		if len(hist) < 2 {
			return "", errors.New("no previous revision")
		}
		target = hist[len(hist)-2].Version
	}
	if dryRun {
		return fmt.Sprintf("ROLLBACK %s/%s from revision %d (%s) to revision %d", a.Namespace, a.Release, cur.Version, chartRef(cur.Chart), target), nil
	}
	rb := action.NewRollback(cfg)
	rb.Version = target
	rb.Timeout = 10 * time.Minute
	rb.MaxHistory = 10
	if err := rb.Run(a.Release); err != nil {
		return "", err
	}
	return fmt.Sprintf("rolled back %s/%s to revision %d", a.Namespace, a.Release, target), nil
}

func (h *Helm) uninstall(raw json.RawMessage, dryRun bool) (string, error) {
	var a struct{ Release, Namespace string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if err := h.check(a.Namespace); err != nil {
		return "", err
	}
	cfg, _, err := h.config(a.Namespace)
	if err != nil {
		return "", err
	}
	if dryRun {
		rel, err := action.NewGet(cfg).Run(a.Release)
		if err != nil {
			return "", err
		}
		res := resources(rel.Manifest)
		var b strings.Builder
		fmt.Fprintf(&b, "UNINSTALL %s/%s (chart %s) — deletes %d resources:\n", a.Namespace, a.Release, chartRef(rel.Chart), len(res))
		for _, r := range sortedKeys(res) {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
		b.WriteString("CRDs installed by the chart are kept.")
		return clip(b.String(), maxOutput), nil
	}
	resp, err := action.NewUninstall(cfg).Run(a.Release)
	if err != nil {
		return "", err
	}
	return "uninstalled " + resp.Release.Name, nil
}

// resources maps "Kind namespace/name" to the rendered document of every object in a manifest.
func resources(manifest string) map[string]string {
	out := map[string]string{}
	for _, doc := range releaseutil.SplitManifests(manifest) {
		var m struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m.Kind == "" {
			continue
		}
		key := m.Kind + " " + m.Metadata.Name
		if m.Metadata.Namespace != "" {
			key = m.Kind + " " + m.Metadata.Namespace + "/" + m.Metadata.Name
		}
		out[key] = doc
	}
	return out
}

func sortedKeys(ms ...map[string]string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range ms {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func chartRef(c *chart.Chart) string {
	if c == nil || c.Metadata == nil {
		return "?"
	}
	return c.Metadata.Name + "-" + c.Metadata.Version
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if required == nil {
		req = []byte("[]")
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":%s,"required":%s}`, props, req))
}

func decode(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
