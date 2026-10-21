package k8s

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseManifests(t *testing.T) {
	objs, err := parseManifests([]byte(`
apiVersion: v1
kind: Namespace
metadata: {name: apps}
---
# comment only
---
apiVersion: v1
kind: List
items:
  - {apiVersion: v1, kind: ConfigMap, metadata: {name: a}}
  - {apiVersion: v1, kind: ConfigMap, metadata: {name: b}}
`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range objs {
		names = append(names, o.GetKind()+"/"+o.GetName())
	}
	if got := strings.Join(names, ","); got != "Namespace/apps,ConfigMap/a,ConfigMap/b" {
		t.Fatalf("got %s", got)
	}
	if _, err := parseManifests([]byte("kind: [")); err == nil {
		t.Fatal("broken yaml accepted")
	}
}

func TestCleanYAMLRedactsSecretsAndDiffShowsChangedKey(t *testing.T) {
	mk := func(pw string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": "db", "namespace": "apps", "resourceVersion": "5", "managedFields": []any{map[string]any{}}},
			"data":     map[string]any{"user": "YWRtaW4=", "password": pw},
		}}
	}
	a, b := cleanYAML(mk("c2VjcmV0")), cleanYAML(mk("bmV3"))
	for _, s := range []string{a, b} {
		if strings.Contains(s, "c2VjcmV0") || strings.Contains(s, "bmV3") || strings.Contains(s, "managedFields") || strings.Contains(s, "resourceVersion") {
			t.Fatalf("not cleaned:\n%s", s)
		}
	}
	d := unifiedDiff(a, b)
	if !strings.Contains(d, "-  password: <redacted:") || !strings.Contains(d, "+  password: <redacted:") || strings.Contains(d, "-  user") {
		t.Fatalf("diff should show only the password change:\n%s", d)
	}
	if unifiedDiff(a, a) != "" {
		t.Fatal("identical objects produced a diff")
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{ProtectedNamespaces: []string{"kube-system"}}
	if p.check("kube-system") == nil || p.check("apps") != nil {
		t.Fatal("policy check wrong")
	}
}
