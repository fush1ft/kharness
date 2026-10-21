package deploy

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRender(t *testing.T) {
	out, err := Render(Options{Image: "localhost:5000/kharness:abc", BasePath: "/kharness", TelegramChats: []string{"1", "2"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`image: "localhost:5000/kharness:abc"`, `TELEGRAM_CHAT_IDS: "1,2"`, `path: "/kharness"`,
		// Without it Headlamp's longer Host(...) && PathPrefix(/) rule wins in Traefik and swallows /kharness.
		`traefik.ingress.kubernetes.io/router.priority:`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
	docs := strings.Split(s, "\n---\n")
	if len(docs) != 11 {
		t.Errorf("got %d documents", len(docs))
	}
	for i, d := range docs {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil || m["kind"] == nil {
			t.Errorf("document %d invalid: %v", i, err)
		}
	}
}
