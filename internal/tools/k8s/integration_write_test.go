package k8s

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Previews only (server-side dry-run): nothing is persisted in the cluster.
func TestIntegrationWritePreviews(t *testing.T) {
	kc := os.Getenv("KHARNESS_IT_KUBECONFIG")
	if kc == "" {
		t.Skip("KHARNESS_IT_KUBECONFIG not set")
	}
	c, err := Load(kc)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]interface {
		Preview(context.Context, json.RawMessage) (string, error)
	}{}
	for _, tl := range WriteTools(c, Policy{ProtectedNamespaces: []string{"kube-system"}}) {
		tools[tl.Name()] = tl.(interface {
			Preview(context.Context, json.RawMessage) (string, error)
		})
	}
	ctx := context.Background()
	manifest, _ := json.Marshal(map[string]string{"yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: kharness-dryrun-probe, namespace: default}\ndata: {a: b}\n"})
	out, err := tools["apply_manifest"].Preview(ctx, manifest)
	if err != nil || !strings.Contains(out, "+ ConfigMap default/kharness-dryrun-probe (new)") {
		t.Fatalf("apply preview: %v\n%s", err, out)
	}
	t.Logf("apply preview:\n%s", out)

	out, err = tools["scale"].Preview(ctx, json.RawMessage(`{"kind":"deployment","name":"headlamp","namespace":"headlamp","replicas":2}`))
	if err != nil || !strings.Contains(out, "replicas 1 → 2") {
		t.Fatalf("scale preview: %v %s", err, out)
	}
	if _, err := tools["rollout_restart"].Preview(ctx, json.RawMessage(`{"kind":"deployment","name":"coredns","namespace":"kube-system"}`)); err == nil {
		t.Fatal("protected namespace not enforced")
	}
	out, err = tools["delete_resource"].Preview(ctx, json.RawMessage(`{"kind":"namespace","name":"headlamp"}`))
	if err != nil || !strings.Contains(out, "EVERYTHING") {
		t.Fatalf("delete preview: %v %s", err, out)
	}
	t.Logf("delete preview:\n%s", out)
}
