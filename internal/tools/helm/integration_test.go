package helm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Client-only dry-run of a fresh install plus the read tools; nothing is changed in the cluster.
func TestIntegrationHelm(t *testing.T) {
	kc := os.Getenv("KHARNESS_IT_KUBECONFIG")
	if kc == "" {
		t.Skip("KHARNESS_IT_KUBECONFIG not set")
	}
	t.Setenv("HELM_CACHE_HOME", t.TempDir())
	t.Setenv("HELM_CONFIG_HOME", t.TempDir())
	t.Setenv("HELM_DATA_HOME", t.TempDir())
	h := &Helm{Kubeconfig: kc, Protected: []string{"kube-system"}}
	ctx := context.Background()
	out, err := h.list(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("helm_list:\n%s", out)

	args := `{"release":"podinfo-probe","chart":"podinfo","repo_url":"https://stefanprodan.github.io/podinfo","namespace":"kharness-probe","values":"replicaCount: 2"}`
	out, err = h.upgradeInstall(ctx, json.RawMessage(args), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "INSTALL kharness-probe/podinfo-probe") || !strings.Contains(out, "+ Deployment kharness-probe/podinfo-probe") {
		t.Fatalf("unexpected preview:\n%s", out)
	}
	t.Logf("install preview:\n%s", out)
	if _, err := h.upgradeInstall(ctx, json.RawMessage(strings.Replace(args, "kharness-probe", "kube-system", 1)), true); err == nil {
		t.Fatal("protected namespace not enforced")
	}
}
