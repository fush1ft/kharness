// Package observe queries Prometheus and Loki once the monitoring stack is installed (see recipes).
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fush1ft/kharness/internal/tools"
)

type Observe struct {
	PrometheusURL string
	LokiURL       string
	HTTP          *http.Client
}

func (o *Observe) Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Func{
			N: "prometheus_query", R: tools.Read,
			D: "Run an instant PromQL query (needs the monitoring recipe installed). Examples: " +
				"`sum by (namespace) (rate(container_cpu_usage_seconds_total[5m]))`, `ALERTS{alertstate=\"firing\"}`.",
			S: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
			F: o.prom,
		},
		&tools.Func{
			N: "loki_query", R: tools.Read,
			D: "Search logs with LogQL (needs the loki recipe installed), e.g. `{namespace=\"apps\"} |= \"error\"`. " +
				"Returns newest lines first.",
			S: json.RawMessage(`{"type":"object","properties":{
				"query":{"type":"string"},
				"since":{"type":"string","description":"Go duration, default 1h"},
				"limit":{"type":"integer","description":"Default 100"}
			},"required":["query"]}`),
			F: o.loki,
		},
	}
}

func (o *Observe) get(ctx context.Context, base, path string, q url.Values, out any) error {
	if base == "" {
		return fmt.Errorf("not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c := o.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w (is the monitoring stack installed and is kharness running in the cluster?)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 400)])))
	}
	return json.Unmarshal(body, out)
}

func (o *Observe) prom(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Query string }
	if err := json.Unmarshal(raw, &a); err != nil || a.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	var r struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := o.get(ctx, o.PrometheusURL, "/api/v1/query", url.Values{"query": {a.Query}}, &r); err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	if len(r.Data.Result) == 0 {
		return "(empty result)", nil
	}
	var b strings.Builder
	for i, s := range r.Data.Result {
		if i == 200 {
			fmt.Fprintf(&b, "…(%d more series)\n", len(r.Data.Result)-200)
			break
		}
		val := ""
		if len(s.Value) == 2 {
			val = fmt.Sprint(s.Value[1])
		}
		fmt.Fprintf(&b, "%s %s\n", labels(s.Metric), val)
	}
	return b.String(), nil
}

func (o *Observe) loki(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Query, Since string
		Limit        int
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	since := time.Hour
	if a.Since != "" {
		d, err := time.ParseDuration(a.Since)
		if err != nil {
			return "", fmt.Errorf("since: %w", err)
		}
		since = d
	}
	if a.Limit <= 0 || a.Limit > 500 {
		a.Limit = 100
	}
	q := url.Values{
		"query": {a.Query}, "limit": {fmt.Sprint(a.Limit)}, "direction": {"backward"},
		"start": {fmt.Sprint(time.Now().Add(-since).UnixNano())}, "end": {fmt.Sprint(time.Now().UnixNano())},
	}
	var r struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := o.get(ctx, o.LokiURL, "/loki/api/v1/query_range", q, &r); err != nil {
		return "", fmt.Errorf("loki: %w", err)
	}
	type line struct {
		ts     string
		stream string
		text   string
	}
	var lines []line
	for _, s := range r.Data.Result {
		src := s.Stream["namespace"] + "/" + s.Stream["pod"]
		if src == "/" {
			src = labels(s.Stream)
		}
		for _, v := range s.Values {
			lines = append(lines, line{v[0], src, v[1]})
		}
	}
	if len(lines) == 0 {
		return "(no log lines)", nil
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].ts > lines[j].ts })
	var b strings.Builder
	for _, l := range lines {
		var ns int64
		fmt.Sscan(l.ts, &ns)
		fmt.Fprintf(&b, "%s %s %s\n", time.Unix(0, ns).Format("01-02 15:04:05"), l.stream, strings.TrimRight(l.text, "\n"))
		if b.Len() > 12000 {
			b.WriteString("…(truncated)\n")
			break
		}
	}
	return b.String(), nil
}

func labels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, m[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}
