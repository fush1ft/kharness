// Package watch turns cluster problems (pod/node informers, Alertmanager webhooks) into incidents: it dedups and
// batches them, notifies every front-end and optionally lets the agent diagnose them. Fixes still need approval.
package watch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/fush1ft/kharness/internal/audit"
	"github.com/fush1ft/kharness/internal/events"
	"github.com/fush1ft/kharness/internal/session"
)

type Problem struct {
	Key     string // dedup key
	Summary string // one line
	Source  string // pods | nodes | alertmanager
}

type Watcher struct {
	Client   kubernetes.Interface
	Bus      *events.Bus
	Sessions *session.Manager
	Audit    *audit.Log

	AutoDiagnose bool
	MaxPerHour   int           // diagnoses per hour (free-tier friendly), default 6
	Cooldown     time.Duration // same problem is not reported again within this window, default 1h
	Window       time.Duration // problems arriving within this window form one incident, default 30s
	// IgnoreNamespaces are not watched (e.g. the harness itself while it restarts).
	IgnoreNamespaces []string

	once      sync.Once
	in        chan Problem
	mu        sync.Mutex
	seen      map[string]time.Time
	diagnoses []time.Time
}

func (w *Watcher) defaults() {
	if w.MaxPerHour == 0 {
		w.MaxPerHour = 6
	}
	if w.Cooldown == 0 {
		w.Cooldown = time.Hour
	}
	if w.Window == 0 {
		w.Window = 30 * time.Second
	}
	w.in = make(chan Problem, 256)
	w.seen = map[string]time.Time{}
}

// Report queues a problem (non-blocking).
func (w *Watcher) Report(p Problem) {
	w.once.Do(w.defaults)
	select {
	case w.in <- p:
	default:
		slog.Warn("watch: queue full, dropping problem", "key", p.Key)
	}
}

// Run starts the informers (if Client is set) and processes problems until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	w.once.Do(w.defaults)
	if w.Client != nil {
		f := informers.NewSharedInformerFactory(w.Client, 5*time.Minute)
		pods := f.Core().V1().Pods().Informer()
		nodes := f.Core().V1().Nodes().Informer()
		onPod := func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok && !slices.Contains(w.IgnoreNamespaces, p.Namespace) {
				for _, pr := range PodProblems(p, time.Now()) {
					w.Report(pr)
				}
			}
		}
		onNode := func(obj any) {
			if n, ok := obj.(*corev1.Node); ok {
				for _, pr := range NodeProblems(n) {
					w.Report(pr)
				}
			}
		}
		if _, err := pods.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: onPod, UpdateFunc: func(_, o any) { onPod(o) }}); err != nil {
			return err
		}
		if _, err := nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: onNode, UpdateFunc: func(_, o any) { onNode(o) }}); err != nil {
			return err
		}
		f.Start(ctx.Done())
	}

	var batch []Problem
	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case p := <-w.in:
			if !w.fresh(p.Key) {
				continue
			}
			if len(batch) == 0 {
				timer = time.After(w.Window)
			}
			batch = append(batch, p)
		case <-timer:
			b := batch
			batch, timer = nil, nil
			w.incident(ctx, b)
		}
	}
}

// fresh records the key and reports whether it was not seen within the cooldown.
func (w *Watcher) fresh(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if t, ok := w.seen[key]; ok && now.Sub(t) < w.Cooldown {
		return false
	}
	w.seen[key] = now
	for k, t := range w.seen { // keep the map small
		if now.Sub(t) > 2*w.Cooldown {
			delete(w.seen, k)
		}
	}
	return true
}

func (w *Watcher) budget() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	w.diagnoses = slices.DeleteFunc(w.diagnoses, func(t time.Time) bool { return t.Before(cutoff) })
	if len(w.diagnoses) >= w.MaxPerHour {
		return false
	}
	w.diagnoses = append(w.diagnoses, time.Now())
	return true
}

func (w *Watcher) incident(ctx context.Context, batch []Problem) {
	if len(batch) == 0 {
		return
	}
	id := newID()
	lines := make([]string, len(batch))
	for i, p := range batch {
		lines[i] = "- " + p.Summary
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	w.Bus.Publish(events.Event{Type: events.Incident, ID: id, Text: summary, By: batch[0].Source})
	w.Audit.Add(audit.Entry{Session: "incident", Kind: "incident", OK: false, Actor: batch[0].Source, Detail: summary})
	slog.Info("incident", "id", id, "problems", len(batch))

	if !w.AutoDiagnose || w.Sessions == nil {
		return
	}
	if !w.budget() {
		w.Bus.Publish(events.Event{Type: events.Notice, Text: fmt.Sprintf("incident %s: automatic diagnosis skipped (limit %d/hour reached)", id, w.MaxPerHour)})
		return
	}
	prompt := "Automatic incident " + id + ". The watcher detected:\n" + summary + "\n\n" +
		"Investigate with read tools, find the root cause, and answer with: cause, evidence, proposed fix. " +
		"If a fix is a clear, low-risk change (restart, rollback, resource bump), call the tool — the operator " +
		"will approve or deny it. Do not delete anything. Keep the report short; it is sent to a phone."
	go func() {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		ans, err := w.Sessions.Ask(dctx, "incident", "watcher", prompt, true)
		if err != nil {
			w.Bus.Publish(events.Event{Type: events.Incident, ID: id, Text: "diagnosis failed: " + err.Error(), By: "agent"})
			return
		}
		w.Bus.Publish(events.Event{Type: events.Incident, ID: id, Text: ans, By: "agent", OK: true})
	}()
}

var badWaiting = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError",
	"CreateContainerError", "InvalidImageName", "RunContainerError"}

// PodProblems inspects one pod. Keys use the owner so replicas of one workload collapse into one problem.
func PodProblems(p *corev1.Pod, now time.Time) []Problem {
	owner := p.Name
	if len(p.OwnerReferences) > 0 {
		owner = p.OwnerReferences[0].Kind + "/" + p.OwnerReferences[0].Name
	}
	where := p.Namespace + "/" + p.Name
	var out []Problem
	statuses := append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if wt := cs.State.Waiting; wt != nil && slices.Contains(badWaiting, wt.Reason) {
			msg := firstLine(wt.Message)
			if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason != "" {
				msg = strings.TrimSpace(fmt.Sprintf("last exit: %s (code %d) %s", cs.LastTerminationState.Terminated.Reason,
					cs.LastTerminationState.Terminated.ExitCode, msg))
			}
			out = append(out, Problem{
				Key:     fmt.Sprintf("pod/%s/%s/%s/%s", p.Namespace, owner, cs.Name, wt.Reason),
				Summary: strings.TrimSpace(fmt.Sprintf("pod %s container %s: %s, %d restarts. %s", where, cs.Name, wt.Reason, cs.RestartCount, msg)),
				Source:  "pods",
			})
			continue
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" && now.Sub(t.FinishedAt.Time) < 10*time.Minute {
			out = append(out, Problem{
				Key:     fmt.Sprintf("pod/%s/%s/%s/OOMKilled", p.Namespace, owner, cs.Name),
				Summary: fmt.Sprintf("pod %s container %s was OOMKilled (%d restarts)", where, cs.Name, cs.RestartCount),
				Source:  "pods",
			})
		}
	}
	if p.Status.Phase == corev1.PodFailed && p.DeletionTimestamp == nil {
		out = append(out, Problem{
			Key:     fmt.Sprintf("pod/%s/%s/Failed", p.Namespace, owner),
			Summary: strings.TrimSpace(fmt.Sprintf("pod %s failed: %s %s", where, p.Status.Reason, firstLine(p.Status.Message))),
			Source:  "pods",
		})
	}
	if p.Status.Phase == corev1.PodPending && now.Sub(p.CreationTimestamp.Time) > 5*time.Minute {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				out = append(out, Problem{
					Key:     fmt.Sprintf("pod/%s/%s/Unschedulable", p.Namespace, owner),
					Summary: fmt.Sprintf("pod %s cannot be scheduled: %s", where, firstLine(c.Message)),
					Source:  "pods",
				})
			}
		}
	}
	return out
}

func NodeProblems(n *corev1.Node) []Problem {
	var out []Problem
	for _, c := range n.Status.Conditions {
		bad := c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue ||
			c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue // *Pressure, NetworkUnavailable
		if bad {
			out = append(out, Problem{
				Key:     fmt.Sprintf("node/%s/%s/%s", n.Name, c.Type, c.Status),
				Summary: strings.TrimSpace(fmt.Sprintf("node %s: %s=%s %s", n.Name, c.Type, c.Status, firstLine(c.Message))),
				Source:  "nodes",
			})
		}
	}
	return out
}

// AlertHandler receives Alertmanager webhooks (POST, JSON). It is served on the in-cluster-only port.
func (w *Watcher) AlertHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var msg struct {
			Alerts []struct {
				Status      string            `json:"status"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				Fingerprint string            `json:"fingerprint"`
			} `json:"alerts"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&msg); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		for _, a := range msg.Alerts {
			desc := a.Annotations["summary"]
			if desc == "" {
				desc = a.Annotations["description"]
			}
			scope := a.Labels["namespace"]
			for _, l := range []string{"pod", "deployment", "statefulset", "daemonset", "job_name", "node", "instance"} {
				if v := a.Labels[l]; v != "" {
					scope = strings.Trim(scope+"/"+v, "/")
					break
				}
			}
			line := strings.TrimSpace(fmt.Sprintf("alert %s [%s] %s: %s", a.Labels["alertname"], a.Labels["severity"], scope, firstLine(desc)))
			if a.Status == "resolved" {
				w.Bus.Publish(events.Event{Type: events.Notice, Text: "resolved: " + line})
				continue
			}
			w.Report(Problem{Key: "alert/" + a.Fingerprint, Summary: line, Source: "alertmanager"})
		}
		rw.WriteHeader(http.StatusNoContent)
	})
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func newID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
