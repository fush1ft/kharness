package k8s

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// Runs every read tool against a real cluster: KHARNESS_IT_KUBECONFIG=~/.kube/server1.yaml go test ./internal/tools/k8s -run Integration -v
func TestIntegrationReadTools(t *testing.T) {
	kc := os.Getenv("KHARNESS_IT_KUBECONFIG")
	if kc == "" {
		t.Skip("KHARNESS_IT_KUBECONFIG not set")
	}
	c, err := Load(kc)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{
		"cluster_overview": `{}`,
		"list_resources":   `{"kind":"deploy"}`,
		"get_resource":     `{"kind":"svc","name":"kubernetes","namespace":"default"}`,
		"list_events":      `{"warnings_only":true}`,
		"top":              `{"target":"nodes"}`,
	}
	for _, tool := range ReadTools(c) {
		a, ok := args[tool.Name()]
		if !ok {
			continue
		}
		out, err := tool.Run(context.Background(), json.RawMessage(a))
		if err != nil {
			t.Errorf("%s: %v", tool.Name(), err)
			continue
		}
		t.Logf("== %s\n%s", tool.Name(), out)
	}
}
