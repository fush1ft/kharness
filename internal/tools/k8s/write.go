package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"

	"github.com/fush1ft/kharness/internal/tools"
)

// FieldManager identifies kharness in server-side apply.
const FieldManager = "kharness"

// Policy holds guardrails enforced by the write tools regardless of approval.
type Policy struct {
	// ProtectedNamespaces cannot be written to (and, as Namespace objects, not deleted).
	ProtectedNamespaces []string
}

func (p Policy) check(ns string) error {
	if slices.Contains(p.ProtectedNamespaces, ns) {
		return fmt.Errorf("namespace %q is protected; kharness will not modify it (change KHARNESS_PROTECTED_NS to allow)", ns)
	}
	return nil
}

// WriteTools returns the cluster-changing tools. They all go through the approval gate (see agent).
func WriteTools(c *Clients, pol Policy) []tools.Tool {
	w := &writer{c: c, pol: pol}
	return []tools.Tool{
		&tools.Func{
			N: "apply_manifest", R: tools.Write,
			D: "Create or update resources from YAML (multi-document allowed) using server-side apply. The operator sees a dry-run diff before approving. Apply CRDs before custom resources that use them.",
			S: schemaOf(`{
				"yaml": {"type": "string", "description": "One or more Kubernetes manifests separated by ---"},
				"namespace": {"type": "string", "description": "Default namespace for namespaced objects without metadata.namespace"}
			}`, []string{"yaml"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.apply(ctx, raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.apply(ctx, raw, true) },
		},
		&tools.Func{
			N: "scale", R: tools.Write,
			D: "Set the replica count of a deployment or statefulset.",
			S: schemaOf(`{
				"kind": {"type": "string", "enum": ["deployment", "statefulset"]},
				"name": {"type": "string"},
				"namespace": {"type": "string"},
				"replicas": {"type": "integer", "minimum": 0}
			}`, []string{"kind", "name", "namespace", "replicas"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.scale(ctx, raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.scale(ctx, raw, true) },
		},
		&tools.Func{
			N: "rollout_restart", R: tools.Write,
			D: "Restart the pods of a deployment, statefulset or daemonset (like kubectl rollout restart).",
			S: schemaOf(`{
				"kind": {"type": "string", "enum": ["deployment", "statefulset", "daemonset"]},
				"name": {"type": "string"},
				"namespace": {"type": "string"}
			}`, []string{"kind", "name", "namespace"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.restart(ctx, raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.restart(ctx, raw, true) },
		},
		&tools.Func{
			N: "delete_resource", R: tools.Dangerous,
			D: "Delete one resource. Deleting a pod owned by a controller just recreates it; prefer rollout_restart.",
			S: schemaOf(`{
				"kind": {"type": "string"},
				"name": {"type": "string"},
				"namespace": {"type": "string"}
			}`, []string{"kind", "name"}),
			F: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.delete(ctx, raw, false) },
			P: func(ctx context.Context, raw json.RawMessage) (string, error) { return w.delete(ctx, raw, true) },
		},
	}
}

type writer struct {
	c   *Clients
	pol Policy

	once   sync.Once
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func (w *writer) restMapper() *restmapper.DeferredDiscoveryRESTMapper {
	w.once.Do(func() { w.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(w.c.Discovery)) })
	return w.mapper
}

// Apply applies manifests with server-side apply; with dryRun it returns a diff instead. Exported for bootstrap.
func (c *Clients) Apply(ctx context.Context, manifests []byte, defaultNS string, pol Policy, dryRun bool) (string, error) {
	return (&writer{c: c, pol: pol}).applyDocs(ctx, manifests, defaultNS, dryRun)
}

func (w *writer) apply(ctx context.Context, raw json.RawMessage, dryRun bool) (string, error) {
	var a struct{ YAML, Namespace string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	return w.applyDocs(ctx, []byte(a.YAML), a.Namespace, dryRun)
}

func (w *writer) applyDocs(ctx context.Context, manifests []byte, defaultNS string, dryRun bool) (string, error) {
	objs, err := parseManifests(manifests)
	if err != nil {
		return "", err
	}
	if len(objs) == 0 {
		return "", errors.New("no objects in yaml")
	}
	if defaultNS == "" {
		defaultNS = "default"
	}
	type target struct {
		obj *unstructured.Unstructured
		ri  dynamic.ResourceInterface
	}
	// Resolve and check everything first so a policy violation in document 3 doesn't leave 1–2 applied.
	targets := make([]target, 0, len(objs))
	for _, obj := range objs {
		m, err := w.mapping(obj)
		if err != nil {
			return "", err
		}
		var ri dynamic.ResourceInterface = w.c.Dynamic.Resource(m.Resource)
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			if obj.GetNamespace() == "" {
				obj.SetNamespace(defaultNS)
			}
			if err := w.pol.check(obj.GetNamespace()); err != nil {
				return "", err
			}
			ri = w.c.Dynamic.Resource(m.Resource).Namespace(obj.GetNamespace())
		} else {
			obj.SetNamespace("")
			if obj.GetKind() == "Namespace" {
				if err := w.pol.check(obj.GetName()); err != nil {
					return "", err
				}
			}
		}
		targets = append(targets, target{obj, ri})
	}

	var out strings.Builder
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: true}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	for _, t := range targets {
		label := objLabel(t.obj)
		var before *unstructured.Unstructured
		if dryRun {
			cur, err := t.ri.Get(ctx, t.obj.GetName(), metav1.GetOptions{})
			switch {
			case err == nil:
				before = cur
			case !apierrors.IsNotFound(err):
				return "", fmt.Errorf("%s: %w", label, err)
			}
		}
		after, err := t.ri.Apply(ctx, t.obj.GetName(), t.obj, opts)
		if err != nil {
			return out.String(), fmt.Errorf("%s: %w", label, err)
		}
		if !dryRun {
			fmt.Fprintf(&out, "applied %s\n", label)
			continue
		}
		if before == nil {
			fmt.Fprintf(&out, "+ %s (new)\n%s\n", label, indent(cleanYAML(after)))
			continue
		}
		d := unifiedDiff(cleanYAML(before), cleanYAML(after))
		if d == "" {
			fmt.Fprintf(&out, "= %s (no change)\n", label)
		} else {
			fmt.Fprintf(&out, "~ %s\n%s\n", label, d)
		}
	}
	return clip(out.String()), nil
}

func (w *writer) mapping(obj *unstructured.Unstructured) (*meta.RESTMapping, error) {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" || obj.GetName() == "" {
		return nil, fmt.Errorf("object without kind or metadata.name (%s)", gvk)
	}
	m, err := w.restMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		w.restMapper().Reset() // CRD may be new
		m, err = w.restMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gvk, err)
	}
	return m, nil
}

func parseManifests(data []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.IsList() {
			l, err := u.ToList()
			if err != nil {
				return nil, err
			}
			for i := range l.Items {
				out = append(out, &l.Items[i])
			}
			continue
		}
		out = append(out, u)
	}
}

func objLabel(u *unstructured.Unstructured) string {
	if u.GetNamespace() != "" {
		return fmt.Sprintf("%s %s/%s", u.GetKind(), u.GetNamespace(), u.GetName())
	}
	return fmt.Sprintf("%s %s", u.GetKind(), u.GetName())
}

// cleanYAML renders an object without server-managed noise; Secret values are replaced by short hashes so a
// diff still shows which keys changed.
func cleanYAML(u *unstructured.Unstructured) string {
	o := u.DeepCopy().Object
	for _, f := range [][]string{
		{"metadata", "managedFields"}, {"metadata", "resourceVersion"}, {"metadata", "uid"},
		{"metadata", "generation"}, {"metadata", "creationTimestamp"}, {"status"},
		{"metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"},
		{"metadata", "annotations", "deployment.kubernetes.io/revision"},
	} {
		unstructured.RemoveNestedField(o, f...)
	}
	if ann, ok, _ := unstructured.NestedMap(o, "metadata", "annotations"); ok && len(ann) == 0 {
		unstructured.RemoveNestedField(o, "metadata", "annotations")
	}
	if u.GetKind() == "Secret" && u.GroupVersionKind().Group == "" {
		for _, field := range []string{"data", "stringData"} {
			if m, ok, _ := unstructured.NestedMap(o, field); ok {
				for k, v := range m {
					sum := sha256.Sum256([]byte(fmt.Sprint(v)))
					m[k] = "<redacted:" + hex.EncodeToString(sum[:4]) + ">"
				}
				_ = unstructured.SetNestedMap(o, m, field)
			}
		}
	}
	b, err := yaml.Marshal(o)
	if err != nil {
		return fmt.Sprintf("<unrenderable: %v>", err)
	}
	return string(b)
}

func unifiedDiff(a, b string) string {
	if a == b {
		return ""
	}
	d, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(a), B: difflib.SplitLines(b), FromFile: "live", ToFile: "applied", Context: 3,
	})
	return strings.TrimRight(d, "\n")
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

type workloadArgs struct {
	Kind, Name, Namespace string
	Replicas              *int64
}

func (w *writer) workload(raw json.RawMessage, allowed ...string) (workloadArgs, dynamic.ResourceInterface, error) {
	var a workloadArgs
	if err := decode(raw, &a); err != nil {
		return a, nil, err
	}
	a.Kind = strings.ToLower(a.Kind)
	if !slices.Contains(allowed, a.Kind) {
		return a, nil, fmt.Errorf("kind must be one of %s", strings.Join(allowed, ", "))
	}
	if a.Name == "" || a.Namespace == "" {
		return a, nil, errors.New("name and namespace are required")
	}
	if err := w.pol.check(a.Namespace); err != nil {
		return a, nil, err
	}
	r, err := w.c.Resolve(a.Kind)
	if err != nil {
		return a, nil, err
	}
	return a, w.c.Dynamic.Resource(r.GVR).Namespace(a.Namespace), nil
}

func (w *writer) scale(ctx context.Context, raw json.RawMessage, dryRun bool) (string, error) {
	a, ri, err := w.workload(raw, "deployment", "statefulset")
	if err != nil {
		return "", err
	}
	if a.Replicas == nil || *a.Replicas < 0 {
		return "", errors.New("replicas must be >= 0")
	}
	cur, err := ri.Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	old, _, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas")
	label := fmt.Sprintf("%s %s/%s", a.Kind, a.Namespace, a.Name)
	if dryRun {
		return fmt.Sprintf("%s: replicas %d → %d", label, old, *a.Replicas), nil
	}
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, *a.Replicas)
	if _, err := ri.Patch(ctx, a.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		return "", err
	}
	return fmt.Sprintf("scaled %s from %d to %d", label, old, *a.Replicas), nil
}

func (w *writer) restart(ctx context.Context, raw json.RawMessage, dryRun bool) (string, error) {
	a, ri, err := w.workload(raw, "deployment", "statefulset", "daemonset")
	if err != nil {
		return "", err
	}
	if _, err := ri.Get(ctx, a.Name, metav1.GetOptions{}); err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s %s/%s", a.Kind, a.Namespace, a.Name)
	if dryRun {
		return fmt.Sprintf("%s: rolling restart of all pods", label), nil
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	if _, err := ri.Patch(ctx, a.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		return "", err
	}
	return "restarted " + label, nil
}

func (w *writer) delete(ctx context.Context, raw json.RawMessage, dryRun bool) (string, error) {
	var a struct{ Kind, Name, Namespace string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	r, err := w.c.Resolve(a.Kind)
	if err != nil {
		return "", err
	}
	var ri dynamic.ResourceInterface = w.c.Dynamic.Resource(r.GVR)
	if r.Namespaced {
		if a.Namespace == "" {
			return "", fmt.Errorf("%s is namespaced, namespace is required", r.GVR.Resource)
		}
		if err := w.pol.check(a.Namespace); err != nil {
			return "", err
		}
		ri = w.c.Dynamic.Resource(r.GVR).Namespace(a.Namespace)
	} else if r.Kind == "Namespace" {
		if err := w.pol.check(a.Name); err != nil {
			return "", err
		}
	}
	cur, err := ri.Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	label := objLabel(cur)
	if dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "DELETE %s (age %s)", label, age(cur.GetCreationTimestamp()))
		for _, o := range cur.GetOwnerReferences() {
			fmt.Fprintf(&b, "\n  owned by %s %s — it will likely be recreated", o.Kind, o.Name)
		}
		switch r.Kind {
		case "Namespace":
			b.WriteString("\n  ⚠ deletes EVERYTHING in the namespace")
		case "PersistentVolumeClaim", "PersistentVolume":
			b.WriteString("\n  ⚠ data on the volume may be lost (local-path reclaim policy is Delete)")
		case "CustomResourceDefinition":
			b.WriteString("\n  ⚠ deletes all custom resources of this type")
		}
		return b.String(), nil
	}
	prop := metav1.DeletePropagationBackground
	if err := ri.Delete(ctx, a.Name, metav1.DeleteOptions{PropagationPolicy: &prop}); err != nil {
		return "", err
	}
	return "deleted " + label, nil
}
