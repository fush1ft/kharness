package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
)

func crashPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", CreationTimestamp: metav1.Now(),
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", RestartCount: 4,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
		}}},
	}
}

func TestPodProblems(t *testing.T) {
	a, b := PodProblems(crashPod("web-abc-1"), time.Now()), PodProblems(crashPod("web-abc-2"), time.Now())
	if len(a) != 1 || len(b) != 1 || a[0].Key != b[0].Key {
		t.Fatalf("replicas of one workload must share a key: %+v %+v", a, b)
	}
	if !strings.Contains(a[0].Summary, "CrashLoopBackOff") || !strings.Contains(a[0].Summary, "code 1") {
		t.Fatalf("summary: %s", a[0].Summary)
	}
	healthy := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if p := PodProblems(healthy, time.Now()); len(p) != 0 {
		t.Fatalf("healthy pod reported: %+v", p)
	}
	oom := crashPod("x")
	oom.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	oom.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}
	if p := PodProblems(oom, time.Now()); len(p) != 1 || !strings.Contains(p[0].Key, "OOMKilled") {
		t.Fatalf("OOM not detected: %+v", p)
	}
}

func TestNodeProblems(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "server1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
	}}}
	p := NodeProblems(n)
	if len(p) != 1 || !strings.Contains(p[0].Summary, "DiskPressure") {
		t.Fatalf("got %+v", p)
	}
}

func TestBatchingDedupAndAlertWebhook(t *testing.T) {
	bus := events.NewBus()
	ch, unsub := bus.Subscribe(16)
	defer unsub()
	log, _ := audit.Open("")
	w := &Watcher{Bus: bus, Audit: log, Window: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	srv := httptest.NewServer(w.AlertHandler())
	defer srv.Close()
	body := `{"alerts":[
		{"status":"firing","fingerprint":"f1","labels":{"alertname":"KubePodCrashLooping","severity":"warning","namespace":"apps","pod":"web-1"},"annotations":{"summary":"Pod is crash looping."}},
		{"status":"firing","fingerprint":"f1","labels":{"alertname":"KubePodCrashLooping"}},
		{"status":"resolved","fingerprint":"f2","labels":{"alertname":"TargetDown"}}]}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("webhook: %v %v", resp, err)
	}
	w.Report(PodProblems(crashPod("web-abc-1"), time.Now())[0])
	w.Report(PodProblems(crashPod("web-abc-2"), time.Now())[0]) // duplicate key

	var incident, resolved string
	deadline := time.After(2 * time.Second)
	for incident == "" || resolved == "" {
		select {
		case e := <-ch:
			switch e.Type {
			case events.Incident:
				incident = e.Text
			case events.Notice:
				resolved = e.Text
			}
		case <-deadline:
			t.Fatalf("incident=%q resolved=%q", incident, resolved)
		}
	}
	if strings.Count(incident, "\n- ") != 1 || !strings.Contains(incident, "KubePodCrashLooping") || !strings.Contains(incident, "apps/web-1") {
		t.Fatalf("expected one batched incident with 2 deduped problems:\n%s", incident)
	}
	if !strings.Contains(resolved, "TargetDown") {
		t.Fatalf("resolved notice: %s", resolved)
	}
}
