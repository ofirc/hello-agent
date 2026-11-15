// Package grafana answers one question during a triage: is this drift actually hurting
// anyone, or is prod simply old?
//
// That distinction decides whether the agent recommends a rollback now or a normal PR,
// so it is worth a round trip. Queries go through Grafana's datasource proxy rather than
// straight to Prometheus, so the agent needs one credential instead of two and inherits
// Grafana's own access control.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/httpx"
)

type Client struct {
	base  string
	token string
	dsUID string
	http  *http.Client
}

func New(base, token, dsUID string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, dsUID: dsUID, http: httpx.Default()}
}

func (c *Client) Configured() bool { return c.base != "" && c.token != "" && c.dsUID != "" }

// Health is the symptom side of the picture, to be read next to the drift findings.
type Health struct {
	Service      string
	Env          string
	ErrorRate    float64 // 5xx as a fraction of all requests, over the window
	LatencyP99Ms float64
	RPS          float64
	Window       time.Duration
	Partial      bool // at least one query returned no data
}

// Symptomatic is the judgement call the triage actually needs. One percent errors is the
// conventional line between "degraded" and "noise".
func (h Health) Symptomatic() bool { return h.ErrorRate > 0.01 }

func (h Health) String() string {
	if h.Partial && h.RPS == 0 {
		return "no metrics for this service"
	}
	return fmt.Sprintf("%.2f%% 5xx, p99 %.0fms, %.1f rps over %s",
		h.ErrorRate*100, h.LatencyP99Ms, h.RPS, h.Window)
}

// ServiceHealth runs three instant queries against the recording rules the stack
// exposes. Keeping them as instant queries rather than ranges keeps the response small
// and the call fast.
func (c *Client) ServiceHealth(ctx context.Context, service, env string, window time.Duration) (Health, error) {
	h := Health{Service: service, Env: env, Window: window}
	if !c.Configured() {
		return h, fmt.Errorf("grafana is not configured (set GRAFANA_BASE_URL, GRAFANA_TOKEN, GRAFANA_DATASOURCE_UID)")
	}
	w := durationToPromRange(window)
	sel := fmt.Sprintf(`service="%s",env="%s"`, service, env)

	errRate, ok1, err := c.instant(ctx, fmt.Sprintf(
		`sum(rate(http_requests_total{%s,code=~"5.."}[%s])) / clamp_min(sum(rate(http_requests_total{%s}[%s])), 0.001)`,
		sel, w, sel, w))
	if err != nil {
		return h, err
	}
	p99, ok2, err := c.instant(ctx, fmt.Sprintf(
		`histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{%s}[%s])) by (le)) * 1000`, sel, w))
	if err != nil {
		return h, err
	}
	rps, ok3, err := c.instant(ctx, fmt.Sprintf(`sum(rate(http_requests_total{%s}[%s]))`, sel, w))
	if err != nil {
		return h, err
	}

	h.ErrorRate, h.LatencyP99Ms, h.RPS = errRate, p99, rps
	h.Partial = !ok1 || !ok2 || !ok3
	return h, nil
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]any `json:"value"` // [unix_ts, "value-as-string"]
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

// instant returns the scalar, and whether any series came back at all. A missing series
// is not an error: a service with no traffic simply has no rate.
func (c *Client) instant(ctx context.Context, query string) (float64, bool, error) {
	q := url.Values{}
	q.Set("query", query)
	endpoint := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/api/v1/query?%s",
		c.base, url.PathEscape(c.dsUID), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	body, err := httpx.Do(c.http, req, "grafana")
	if err != nil {
		return 0, false, err
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, false, fmt.Errorf("grafana: decode prometheus response: %w", err)
	}
	if pr.Status != "success" {
		return 0, false, fmt.Errorf("grafana: prometheus query failed: %s", pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		return 0, false, nil
	}

	s, _ := pr.Data.Result[0].Value[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, nil // NaN and +Inf arrive as strings, treat as no data
	}
	return v, true, nil
}

// DashboardURL deep-links the humans to the dashboard, because the agent's job is to
// start the investigation, not to replace the graph.
func (c *Client) DashboardURL(service, env string) string {
	if c.base == "" {
		return ""
	}
	q := url.Values{}
	q.Set("var-service", service)
	q.Set("var-env", env)
	q.Set("from", "now-6h")
	q.Set("to", "now")
	return c.base + "/d/service-overview/service-overview?" + q.Encode()
}

func durationToPromRange(d time.Duration) string {
	if d < time.Minute {
		d = time.Minute
	}
	if d >= time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Minutes())) + "m"
}
