package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMatch(t *testing.T) {
	res := []apiResource{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, Kind: "Pod", Singular: "pod", ShortNames: []string{"po"}, Namespaced: true},
		{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Singular: "deployment", ShortNames: []string{"deploy"}},
		{GVR: schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}, Kind: "IngressRoute", Singular: "ingressroute"},
	}
	for in, want := range map[string]string{
		"po": "pods", "Pod": "pods", "pods": "pods", "Deployment": "deployments", "deploy": "deployments",
		"ingressroutes.traefik.io": "ingressroutes", " IngressRoute ": "ingressroutes",
	} {
		r, ok := match(res, in)
		if !ok || r.GVR.Resource != want {
			t.Errorf("match(%q) = %v %v, want %s", in, r.GVR.Resource, ok, want)
		}
	}
	if _, ok := match(res, "deployments.traefik.io"); ok {
		t.Error("group filter ignored")
	}
}

func TestOverviewListsUnhealthyPods(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "server1"}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		pod("ok", corev1.PodRunning, true, nil),
		pod("crash", corev1.PodRunning, false, &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}),
	)
	c := &Clients{Typed: cs}
	out, err := c.overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 total, 1 unhealthy") || !strings.Contains(out, "CrashLoopBackOff") || strings.Contains(out, " ok ") {
		t.Fatalf("unexpected overview:\n%s", out)
	}
}

func pod(name string, phase corev1.PodPhase, ready bool, waiting *corev1.ContainerStateWaiting) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase:             phase,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: ready, State: corev1.ContainerState{Waiting: waiting}}},
		},
	}
}
