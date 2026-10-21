// Package recipes holds step-by-step guides the agent follows to set up cluster components.
package recipes

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fush1ft/kharness/internal/tools"
)

//go:embed files/*.md
var files embed.FS

// Names lists the available recipes.
func Names() []string {
	entries, _ := files.ReadDir("files")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}

func Get(name string) (string, error) {
	b, err := files.ReadFile("files/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("unknown recipe %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return string(b), nil
}

func Tool() tools.Tool {
	enum, _ := json.Marshal(Names())
	return &tools.Func{
		N: "get_recipe", R: tools.Read,
		D: "Get a step-by-step recipe for setting up a cluster component: " + strings.Join(Names(), ", ") +
			". Read it before installing that component and follow its values.",
		S: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"name":{"type":"string","enum":%s}},"required":["name"]}`, enum)),
		F: func(_ context.Context, raw json.RawMessage) (string, error) {
			var a struct{ Name string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
			return Get(a.Name)
		},
	}
}
