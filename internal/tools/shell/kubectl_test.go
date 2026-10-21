package shell

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fush1ft/kharness/internal/tools"
)

func TestClassify(t *testing.T) {
	cases := map[string]tools.Risk{
		"get pods -n apps":                  tools.Read,
		"-n apps get pods":                  tools.Read,
		"rollout history deploy/web":        tools.Read,
		"rollout undo deploy/web -n apps":   tools.Write,
		"auth can-i list pods":              tools.Read,
		"cordon server1":                    tools.Write,
		"label ns apps team=me":             tools.Write,
		"delete pod web-1 -n apps":          tools.Dangerous,
		"drain server1 --ignore-daemonsets": tools.Dangerous,
	}
	for cmd, want := range cases {
		got, err := classify(strings.Fields(cmd))
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", cmd, got, err, want)
		}
	}
	for _, bad := range []string{"exec -it web -- sh", "port-forward svc/x 80", "edit deploy/x", "get pods --kubeconfig=/x",
		"get pods --token abc", "config view", "rollout bogus deploy/x", "proxy"} {
		if _, err := classify(strings.Fields(bad)); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestValidateGuards(t *testing.T) {
	k := &Kubectl{Protected: []string{"kube-system"}}
	call := func(args []string, stdin string) error {
		raw, _ := json.Marshal(map[string]any{"args": args, "stdin": stdin})
		_, err := k.validate(raw)
		return err
	}
	if err := call([]string{"delete", "pod", "x", "-n", "kube-system"}, ""); err == nil {
		t.Error("write to protected namespace allowed")
	}
	if err := call([]string{"get", "pods", "-n", "kube-system"}, ""); err != nil {
		t.Errorf("read of protected namespace refused: %v", err)
	}
	if err := call([]string{"apply", "-f", "-"}, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: kube-system}\n"); err == nil {
		t.Error("stdin manifest into protected namespace allowed")
	}
	if err := call([]string{"get", "secret", "db", "-o", "yaml"}, ""); err == nil {
		t.Error("secret contents exposed")
	}
	if err := call([]string{"get", "secrets", "-n", "apps"}, ""); err != nil {
		t.Errorf("listing secrets refused: %v", err)
	}
	if err := call([]string{"label", "pods", "--all", "x=y", "-A"}, ""); err == nil {
		t.Error("all-namespace change allowed")
	}
	raw, _ := json.Marshal(map[string]any{"args": []string{"kubectl", "delete", "ns", "x"}})
	if r := k.RiskFor(raw); r != tools.Dangerous {
		t.Errorf("leading kubectl not stripped, risk %v", r)
	}
}
