package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

// schemaOf builds a JSON Schema object from a properties literal.
func schemaOf(props string, required []string) json.RawMessage {
	if strings.TrimSpace(props) == "" {
		props = "{}"
	}
	req, _ := json.Marshal(required)
	if required == nil {
		req = []byte("[]")
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":%s,"required":%s}`, props, req))
}

// decode parses tool arguments. Field names match case-insensitively, so snake_case JSON keys need json tags
// only when they contain underscores. Empty arguments ("" or "null") are accepted for tools without params.
func decode(raw json.RawMessage, v any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func clip(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n…(truncated, narrow the query with namespace/label_selector)"
}

func age(t metav1.Time) string {
	if t.IsZero() {
		return "?"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

func fromUnstructured(u *unstructured.Unstructured, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

// podStatus mirrors the STATUS column of kubectl get pods closely enough for diagnosis.
func podStatus(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return "Init:" + w.Reason
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return "Init:" + t.Reason
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason
		}
		if t := cs.State.Terminated; t != nil && t.Reason != "" && p.Status.Phase != corev1.PodSucceeded {
			return t.Reason
		}
	}
	if p.Status.Phase == corev1.PodSucceeded {
		return "Completed"
	}
	if p.Status.Reason != "" {
		return p.Status.Reason
	}
	return string(p.Status.Phase)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func readyCount(p *corev1.Pod) string {
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
}

func restarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

// genericStatus gives a one-word status for any object: replicas for workloads, Ready condition otherwise.
func genericStatus(u *unstructured.Unstructured) string {
	if want, ok, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); ok {
		ready, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
		return fmt.Sprintf("%d/%d ready", ready, want)
	}
	if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok {
		return phase
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" || m["type"] == "Available" {
			return fmt.Sprintf("%v=%v", m["type"], m["status"])
		}
	}
	return "-"
}

// fmtCPU renders a metrics quantity ("53790146n") as millicores; unparsable values pass through.
func fmtCPU(q string) string {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return q
	}
	return fmt.Sprintf("%dm", v.MilliValue())
}

// fmtMem renders a metrics quantity ("1824656Ki") in MiB.
func fmtMem(q string) string {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return q
	}
	return fmt.Sprintf("%dMi", v.Value()/(1<<20))
}
