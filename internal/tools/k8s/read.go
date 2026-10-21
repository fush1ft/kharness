package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"

	"github.com/fush1ft/kharness/internal/tools"
)

// maxOutput caps tool output so a huge list or log doesn't blow the model's context window.
const maxOutput = 12000

// ReadTools returns the side-effect-free cluster tools.
func ReadTools(c *Clients) []tools.Tool {
	return []tools.Tool{
		&tools.Func{
			N: "cluster_overview", R: tools.Read,
			D: "Summary of the cluster: nodes with status/version, namespaces, and every pod that is not healthy. Start here.",
			S: schemaOf(`{}`, nil),
			F: func(ctx context.Context, _ json.RawMessage) (string, error) { return c.overview(ctx) },
		},
		&tools.Func{
			N: "list_resources", R: tools.Read,
			D: "List resources of any kind (pods, deployments, svc, ingressroutes.traefik.io, ...). Omit namespace for all namespaces.",
			S: schemaOf(`{
				"kind": {"type": "string", "description": "Resource kind, plural, or short name"},
				"namespace": {"type": "string"},
				"label_selector": {"type": "string", "description": "e.g. app=web"}
			}`, []string{"kind"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Kind, Namespace, LabelSelector string }
				if err := decode(raw, &a); err != nil {
					return "", err
				}
				return c.list(ctx, a.Kind, a.Namespace, a.LabelSelector)
			},
		},
		&tools.Func{
			N: "get_resource", R: tools.Read,
			D: "Get the full YAML (spec + status) of one resource.",
			S: schemaOf(`{
				"kind": {"type": "string"},
				"name": {"type": "string"},
				"namespace": {"type": "string", "description": "Required for namespaced kinds"}
			}`, []string{"kind", "name"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Kind, Name, Namespace string }
				if err := decode(raw, &a); err != nil {
					return "", err
				}
				return c.get(ctx, a.Kind, a.Name, a.Namespace)
			},
		},
		&tools.Func{
			N: "pod_logs", R: tools.Read,
			D: "Tail logs of a pod container. Use previous=true for the crashed instance of a restarting container.",
			S: schemaOf(`{
				"name": {"type": "string"},
				"namespace": {"type": "string"},
				"container": {"type": "string", "description": "Needed only for multi-container pods"},
				"tail_lines": {"type": "integer", "description": "Default 100"},
				"previous": {"type": "boolean"}
			}`, []string{"name", "namespace"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Name, Namespace, Container string
					TailLines                  int64 `json:"tail_lines"`
					Previous                   bool
				}
				if err := decode(raw, &a); err != nil {
					return "", err
				}
				return c.logs(ctx, a.Name, a.Namespace, a.Container, a.TailLines, a.Previous)
			},
		},
		&tools.Func{
			N: "list_events", R: tools.Read,
			D: "Recent events, newest last. Filter by namespace and/or the name of the involved object.",
			S: schemaOf(`{
				"namespace": {"type": "string"},
				"object_name": {"type": "string"},
				"warnings_only": {"type": "boolean"}
			}`, nil),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace    string
					ObjectName   string `json:"object_name"`
					WarningsOnly bool   `json:"warnings_only"`
				}
				if err := decode(raw, &a); err != nil {
					return "", err
				}
				return c.events(ctx, a.Namespace, a.ObjectName, a.WarningsOnly)
			},
		},
		&tools.Func{
			N: "top", R: tools.Read,
			D: "CPU/memory usage from metrics-server for nodes or pods.",
			S: schemaOf(`{
				"target": {"type": "string", "enum": ["nodes", "pods"]},
				"namespace": {"type": "string"}
			}`, []string{"target"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Target, Namespace string }
				if err := decode(raw, &a); err != nil {
					return "", err
				}
				return c.top(ctx, a.Target, a.Namespace)
			},
		},
	}
}

func (c *Clients) overview(ctx context.Context) (string, error) {
	var b strings.Builder
	nodes, err := c.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tREADY\tVERSION\tOS\tAGE")
	for _, n := range nodes.Items {
		ready := "Unknown"
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = string(cond.Status)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n.Name, ready, n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage, age(n.CreationTimestamp))
	}
	w.Flush()

	nss, err := c.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(nss.Items))
	for _, ns := range nss.Items {
		names = append(names, ns.Name)
	}
	fmt.Fprintf(&b, "\nNamespaces: %s\n", strings.Join(names, ", "))

	pods, err := c.Typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var bad []corev1.Pod
	for _, p := range pods.Items {
		st := podStatus(&p)
		notReady := p.Status.Phase == corev1.PodRunning && !podReady(&p)
		if (st != "Running" && st != "Completed") || notReady {
			bad = append(bad, p)
		}
	}
	fmt.Fprintf(&b, "\nPods: %d total, %d unhealthy\n", len(pods.Items), len(bad))
	if len(bad) > 0 {
		w = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAMESPACE\tPOD\tSTATUS\tRESTARTS\tAGE")
		for _, p := range bad {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", p.Namespace, p.Name, podStatus(&p), restarts(&p), age(p.CreationTimestamp))
		}
		w.Flush()
	}
	return b.String(), nil
}

func (c *Clients) list(ctx context.Context, kind, ns, selector string) (string, error) {
	r, err := c.Resolve(kind)
	if err != nil {
		return "", err
	}
	opts := metav1.ListOptions{LabelSelector: selector}
	var list *unstructured.UnstructuredList
	if r.Namespaced {
		list, err = c.Dynamic.Resource(r.GVR).Namespace(ns).List(ctx, opts)
	} else {
		list, err = c.Dynamic.Resource(r.GVR).List(ctx, opts)
	}
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return fmt.Sprintf("No %s found.", r.GVR.Resource), nil
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	isPod := r.GVR.Group == "" && r.GVR.Resource == "pods"
	switch {
	case isPod:
		fmt.Fprintln(w, "NAMESPACE\tNAME\tREADY\tSTATUS\tRESTARTS\tNODE\tAGE")
	case r.Namespaced:
		fmt.Fprintln(w, "NAMESPACE\tNAME\tSTATUS\tAGE")
	default:
		fmt.Fprintln(w, "NAME\tSTATUS\tAGE")
	}
	for _, it := range list.Items {
		created := it.GetCreationTimestamp()
		if isPod {
			var p corev1.Pod
			if err := fromUnstructured(&it, &p); err == nil {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", p.Namespace, p.Name, readyCount(&p), podStatus(&p), restarts(&p), p.Spec.NodeName, age(created))
				continue
			}
		}
		if r.Namespaced {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.GetNamespace(), it.GetName(), genericStatus(&it), age(created))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\n", it.GetName(), genericStatus(&it), age(created))
		}
	}
	w.Flush()
	return clip(b.String()), nil
}

func (c *Clients) get(ctx context.Context, kind, name, ns string) (string, error) {
	r, err := c.Resolve(kind)
	if err != nil {
		return "", err
	}
	var obj *unstructured.Unstructured
	if r.Namespaced {
		if ns == "" {
			return "", fmt.Errorf("%s is namespaced, namespace is required", r.GVR.Resource)
		}
		obj, err = c.Dynamic.Resource(r.GVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = c.Dynamic.Resource(r.GVR).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", err
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	// Secrets: show keys only, never values.
	if r.GVR.Group == "" && r.GVR.Resource == "secrets" {
		for _, field := range []string{"data", "stringData"} {
			if m, ok, _ := unstructured.NestedMap(obj.Object, field); ok {
				for k := range m {
					m[k] = "<redacted>"
				}
				_ = unstructured.SetNestedMap(obj.Object, m, field)
			}
		}
		unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	}
	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		return "", err
	}
	return clip(string(out)), nil
}

func (c *Clients) logs(ctx context.Context, name, ns, container string, tail int64, previous bool) (string, error) {
	if tail <= 0 {
		tail = 100
	}
	req := c.Typed.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{Container: container, TailLines: &tail, Previous: previous})
	rc, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 4*maxOutput))
	if err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "(no log output)", nil
	}
	// Keep the tail: the end of a log is where crashes are.
	s := string(raw)
	if len(s) > maxOutput {
		s = "…(truncated)\n" + s[len(s)-maxOutput:]
	}
	return s, nil
}

func (c *Clients) events(ctx context.Context, ns, object string, warningsOnly bool) (string, error) {
	opts := metav1.ListOptions{}
	var sel []string
	if object != "" {
		sel = append(sel, "involvedObject.name="+object)
	}
	if warningsOnly {
		sel = append(sel, "type=Warning")
	}
	opts.FieldSelector = strings.Join(sel, ",")
	evs, err := c.Typed.CoreV1().Events(ns).List(ctx, opts)
	if err != nil {
		return "", err
	}
	items := evs.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(&items[i]).Before(eventTime(&items[j])) })
	if len(items) > 60 {
		items = items[len(items)-60:]
	}
	if len(items) == 0 {
		return "No events.", nil
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LAST SEEN\tTYPE\tREASON\tOBJECT\tCOUNT\tMESSAGE")
	for _, e := range items {
		obj := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name
		if ns == "" {
			obj = e.Namespace + "/" + obj
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", duration.HumanDuration(time.Since(eventTime(&e))), e.Type, e.Reason, obj, e.Count, strings.TrimSpace(e.Message))
	}
	w.Flush()
	return clip(b.String()), nil
}

func (c *Clients) top(ctx context.Context, target, ns string) (string, error) {
	gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: target}
	var list *unstructured.UnstructuredList
	var err error
	switch target {
	case "nodes":
		list, err = c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	case "pods":
		list, err = c.Dynamic.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	default:
		return "", fmt.Errorf("target must be nodes or pods")
	}
	if err != nil {
		return "", fmt.Errorf("metrics API unavailable (is metrics-server running?): %w", err)
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCPU\tMEMORY")
	for _, it := range list.Items {
		name := it.GetName()
		if target == "pods" {
			name = it.GetNamespace() + "/" + name
			containers, _, _ := unstructured.NestedSlice(it.Object, "containers")
			var cpu, mem []string
			for _, ct := range containers {
				m, _ := ct.(map[string]any)
				u, _ := m["usage"].(map[string]any)
				cpu = append(cpu, fmtCPU(fmt.Sprint(u["cpu"])))
				mem = append(mem, fmtMem(fmt.Sprint(u["memory"])))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", name, strings.Join(cpu, "+"), strings.Join(mem, "+"))
			continue
		}
		cpu, _, _ := unstructured.NestedString(it.Object, "usage", "cpu")
		mem, _, _ := unstructured.NestedString(it.Object, "usage", "memory")
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, fmtCPU(cpu), fmtMem(mem))
	}
	w.Flush()
	return clip(b.String()), nil
}
