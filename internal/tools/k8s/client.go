// Package k8s holds the typed Kubernetes tools the agent uses (client-go based).
package k8s

import (
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Clients struct {
	Config    *rest.Config
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface

	mu        sync.Mutex
	resources []apiResource // cached discovery result
}

type apiResource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Singular   string
	ShortNames []string
	Namespaced bool
}

// Load builds clients from an explicit kubeconfig path, falling back to $KUBECONFIG / ~/.kube/config and then to
// the in-cluster service account.
func Load(kubeconfig string) (*Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		var inErr error
		if cfg, inErr = rest.InClusterConfig(); inErr != nil {
			return nil, fmt.Errorf("no kubeconfig (%v) and not in cluster (%v)", err, inErr)
		}
	}
	return NewClients(cfg)
}

func NewClients(cfg *rest.Config) (*Clients, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Config: cfg, Typed: typed, Dynamic: dyn, Discovery: typed.Discovery()}, nil
}

// Resolve maps a user/LLM supplied kind ("pods", "po", "Deployment", "ingressroutes.traefik.io") to a resource.
func (c *Clients) Resolve(kind string) (apiResource, error) {
	res, err := c.discover(false)
	if err != nil {
		return apiResource{}, err
	}
	if r, ok := match(res, kind); ok {
		return r, nil
	}
	// A CRD may have been installed since the cache was filled.
	if res, err = c.discover(true); err != nil {
		return apiResource{}, err
	}
	if r, ok := match(res, kind); ok {
		return r, nil
	}
	return apiResource{}, fmt.Errorf("unknown resource kind %q", kind)
}

func match(res []apiResource, kind string) (apiResource, bool) {
	k := strings.ToLower(strings.TrimSpace(kind))
	name, group, hasGroup := strings.Cut(k, ".")
	for _, r := range res {
		if hasGroup && r.GVR.Group != group {
			continue
		}
		if name == r.GVR.Resource || name == r.Singular || name == strings.ToLower(r.Kind) {
			return r, true
		}
		for _, s := range r.ShortNames {
			if name == s {
				return r, true
			}
		}
	}
	return apiResource{}, false
}

func (c *Clients) discover(refresh bool) ([]apiResource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resources != nil && !refresh {
		return c.resources, nil
	}
	lists, err := c.Discovery.ServerPreferredResources()
	// Partial failures (e.g. a broken metrics APIService) still return usable lists.
	if err != nil && len(lists) == 0 {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var out []apiResource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") { // subresources
				continue
			}
			singular := r.SingularName
			if singular == "" {
				singular = strings.ToLower(r.Kind)
			}
			out = append(out, apiResource{
				GVR: gv.WithResource(r.Name), Kind: r.Kind, Singular: singular,
				ShortNames: r.ShortNames, Namespaced: r.Namespaced,
			})
		}
	}
	c.resources = out
	return out, nil
}
