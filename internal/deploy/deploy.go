// Package deploy installs kharness itself into the cluster (`kharness bootstrap`).
package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fush1ft/kharness/internal/tools/k8s"
)

//go:embed manifests.yaml
var manifests string

const (
	Namespace  = "kharness"
	SecretName = "kharness-secrets"
)

type Options struct {
	Image         string
	BasePath      string
	Models        []string
	TelegramChats []string

	OpenRouterKey string // required on first install; kept from the existing secret when empty
	TelegramToken string // optional; kept from the existing secret when empty
	DryRun        bool
}

// Render returns the manifests for the given options.
func Render(o Options) ([]byte, error) {
	t, err := template.New("manifests").Option("missingkey=error").Parse(manifests)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{
		"Image": o.Image, "BasePath": o.BasePath,
		"Models": strings.Join(o.Models, ","), "TelegramChats": strings.Join(o.TelegramChats, ","),
	})
	return b.Bytes(), err
}

// Result is what the operator needs after a bootstrap.
type Result struct {
	Applied string // apply output or dry-run diff
	Token   string // API token for the TUI
}

// Run applies manifests and the secret, restarts the deployment and waits for it to become ready.
func Run(ctx context.Context, c *k8s.Clients, o Options, progress func(string)) (Result, error) {
	var res Result
	data, err := Render(o)
	if err != nil {
		return res, err
	}
	progress("applying manifests")
	out, err := c.Apply(ctx, data, Namespace, k8s.Policy{}, o.DryRun)
	res.Applied = out
	if err != nil {
		return res, err
	}

	secrets := c.Typed.CoreV1().Secrets(Namespace)
	cur, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return res, err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace}, Data: map[string][]byte{}}
	if exists {
		sec = cur.DeepCopy()
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
	}
	if o.OpenRouterKey != "" {
		sec.Data["OPENROUTER_API_KEY"] = []byte(o.OpenRouterKey)
	}
	if len(sec.Data["OPENROUTER_API_KEY"]) == 0 {
		return res, fmt.Errorf("OPENROUTER_API_KEY is required for the first install")
	}
	if o.TelegramToken != "" {
		sec.Data["TELEGRAM_BOT_TOKEN"] = []byte(o.TelegramToken)
	}
	if len(sec.Data["KHARNESS_TOKEN"]) == 0 {
		sec.Data["KHARNESS_TOKEN"] = []byte(randomToken())
	}
	res.Token = string(sec.Data["KHARNESS_TOKEN"])
	if o.DryRun {
		verb := map[bool]string{true: "update", false: "create"}[exists]
		res.Applied += fmt.Sprintf("~ Secret %s/%s (%s; keys: %s)\n", Namespace, SecretName, verb, keys(sec.Data))
		return res, nil
	}
	progress("writing secret")
	if exists {
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{FieldManager: k8s.FieldManager})
	} else {
		_, err = secrets.Create(ctx, sec, metav1.CreateOptions{FieldManager: k8s.FieldManager})
	}
	if err != nil {
		return res, err
	}

	// The pod reads config and secret only at start, so always roll it.
	progress("restarting kharness")
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, time.Now().Format(time.RFC3339))
	deps := c.Typed.AppsV1().Deployments(Namespace)
	if _, err := deps.Patch(ctx, "kharness", types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: k8s.FieldManager}); err != nil {
		return res, err
	}

	progress("waiting for the pod to become ready")
	deadline := time.Now().Add(4 * time.Minute)
	for {
		d, err := deps.Get(ctx, "kharness", metav1.GetOptions{})
		if err != nil {
			return res, err
		}
		st := d.Status
		if st.ObservedGeneration >= d.Generation && st.UpdatedReplicas == 1 && st.ReadyReplicas == 1 && st.Replicas == 1 {
			return res, nil
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("kharness did not become ready in time; check `kubectl -n kharness describe pod -l app=kharness` " +
				"(an ImagePullBackOff means the image is not in the node-local registry — run make push)")
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func keys(m map[string][]byte) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ", ")
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
